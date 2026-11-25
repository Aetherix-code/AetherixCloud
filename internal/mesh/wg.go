package mesh

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// DefaultInterface and DefaultPort for the overlay.
const (
	DefaultInterface = "ax0"
	DefaultPort      = 51820
)

// KeyPair is a WireGuard key pair (base64 strings).
type KeyPair struct {
	Private string
	Public  string
}

// GenerateKeyPair creates a new WireGuard key pair.
func GenerateKeyPair() (KeyPair, error) {
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Private: k.String(), Public: k.PublicKey().String()}, nil
}

// Peer is a remote machine.
type Peer struct {
	ID        string
	PublicKey string
	// Endpoints are candidate "host:port" UDP endpoints, tried in order until
	// a handshake succeeds.
	Endpoints  []string
	AllowedIPs []netip.Prefix
	Keepalive  time.Duration
}

// Config configures the local WireGuard interface.
type Config struct {
	Interface  string
	PrivateKey string
	ListenPort int
	Address    netip.Prefix // e.g. 10.210.3.1/16
	MTU        int
	Peers      []Peer
}

// PeersFromMachines builds the peer list for self from store machines.
func PeersFromMachines(self string, machines []*v1.Machine) []Peer {
	var out []Peer
	for _, m := range machines {
		if m.Id == self || m.PublicKey == "" {
			continue
		}
		p := Peer{ID: m.Id, PublicKey: m.PublicKey, Endpoints: m.Endpoints, Keepalive: 25 * time.Second}
		if sn, err := netip.ParsePrefix(m.Subnet); err == nil {
			p.AllowedIPs = []netip.Prefix{sn}
		} else {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// WG is the subset of wgctrl.Client the manager uses (fakeable).
type WG interface {
	ConfigureDevice(name string, cfg wgtypes.Config) error
	Device(name string) (*wgtypes.Device, error)
	Close() error
}

// Link creates and tears down the network interface backing the tunnel.
type Link interface {
	// Ensure creates the interface (if needed), assigns addr, sets MTU and
	// brings it up. It reports which implementation is in use.
	Ensure(ctx context.Context, name string, addr netip.Prefix, mtu int) (impl string, err error)
	Delete(name string) error
}

// Manager owns the WireGuard device.
type Manager struct {
	cfg  Config
	link Link
	wg   WG

	// RotateAfter is how long a peer may lack a handshake before the next
	// candidate endpoint is tried.
	RotateAfter time.Duration
	Now         func() time.Time

	mu    sync.Mutex
	peers map[string]*peerState // by public key
	impl  string
}

type peerState struct {
	peer     Peer
	idx      int
	switched time.Time
}

// New creates a Manager with injected backends (tests).
func New(cfg Config, link Link, wg WG) *Manager {
	if cfg.Interface == "" {
		cfg.Interface = DefaultInterface
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = DefaultPort
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1380
	}
	return &Manager{cfg: cfg, link: link, wg: wg, RotateAfter: 30 * time.Second, Now: time.Now, peers: map[string]*peerState{}}
}

// NewSystem creates a Manager using the host's WireGuard (kernel module via
// netlink on Linux, wireguard-go otherwise). It errors on unsupported OSes.
func NewSystem(cfg Config) (*Manager, error) {
	link, err := newSystemLink()
	if err != nil {
		return nil, err
	}
	c, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("mesh: wgctrl: %w", err)
	}
	return New(cfg, link, c), nil
}

// Implementation returns "kernel" or "userspace" after Up.
func (m *Manager) Implementation() string { m.mu.Lock(); defer m.mu.Unlock(); return m.impl }

// Up creates the interface and applies the configuration (including peers).
func (m *Manager) Up(ctx context.Context) error {
	impl, err := m.link.Ensure(ctx, m.cfg.Interface, m.cfg.Address, m.cfg.MTU)
	if err != nil {
		return fmt.Errorf("mesh: create interface: %w", err)
	}
	m.mu.Lock()
	m.impl = impl
	m.mu.Unlock()
	key, err := wgtypes.ParseKey(m.cfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("mesh: private key: %w", err)
	}
	port := m.cfg.ListenPort
	if err := m.wg.ConfigureDevice(m.cfg.Interface, wgtypes.Config{PrivateKey: &key, ListenPort: &port}); err != nil {
		return fmt.Errorf("mesh: configure device: %w", err)
	}
	return m.SetPeers(ctx, m.cfg.Peers)
}

// Down removes the interface.
func (m *Manager) Down() error {
	_ = m.wg.Close()
	return m.link.Delete(m.cfg.Interface)
}

func udpAddr(ep string) (*net.UDPAddr, error) { return net.ResolveUDPAddr("udp", ep) }

// SetPeers makes the device's peer set equal to peers (add/update/remove).
func (m *Manager) SetPeers(ctx context.Context, peers []Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var pcs []wgtypes.PeerConfig
	want := map[string]bool{}
	for _, p := range peers {
		key, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("mesh: peer %s key: %w", p.ID, err)
		}
		want[p.PublicKey] = true
		st, ok := m.peers[p.PublicKey]
		if !ok || !equalStrings(st.peer.Endpoints, p.Endpoints) {
			st = &peerState{peer: p, switched: m.Now()}
			m.peers[p.PublicKey] = st
		}
		st.peer = p
		pc := wgtypes.PeerConfig{PublicKey: key, ReplaceAllowedIPs: true}
		if len(p.Endpoints) > 0 {
			if a, err := udpAddr(p.Endpoints[st.idx%len(p.Endpoints)]); err == nil {
				pc.Endpoint = a
			}
		}
		if p.Keepalive > 0 {
			ka := p.Keepalive
			pc.PersistentKeepaliveInterval = &ka
		}
		for _, ip := range p.AllowedIPs {
			pc.AllowedIPs = append(pc.AllowedIPs, net.IPNet{IP: ip.Masked().Addr().AsSlice(), Mask: net.CIDRMask(ip.Bits(), ip.Addr().BitLen())})
		}
		pcs = append(pcs, pc)
	}
	// removals
	dev, err := m.wg.Device(m.cfg.Interface)
	if err == nil {
		for _, dp := range dev.Peers {
			if !want[dp.PublicKey.String()] {
				pcs = append(pcs, wgtypes.PeerConfig{PublicKey: dp.PublicKey, Remove: true})
			}
		}
	}
	for k := range m.peers {
		if !want[k] {
			delete(m.peers, k)
		}
	}
	return m.wg.ConfigureDevice(m.cfg.Interface, wgtypes.Config{Peers: pcs})
}

// Run rotates endpoints of peers that never complete a handshake until ctx
// is done. It is the NAT fallback: a peer advertises several candidate
// endpoints (public IP, LAN IP, ...) and we try each in turn.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = m.RotateStale()
		}
	}
}

// RotateStale advances the endpoint of peers without a recent handshake.
func (m *Manager) RotateStale() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dev, err := m.wg.Device(m.cfg.Interface)
	if err != nil {
		return err
	}
	now := m.Now()
	var pcs []wgtypes.PeerConfig
	for _, dp := range dev.Peers {
		st, ok := m.peers[dp.PublicKey.String()]
		if !ok || len(st.peer.Endpoints) < 2 {
			continue
		}
		fresh := !dp.LastHandshakeTime.IsZero() && now.Sub(dp.LastHandshakeTime) < m.RotateAfter
		if fresh || now.Sub(st.switched) < m.RotateAfter {
			continue
		}
		st.idx = (st.idx + 1) % len(st.peer.Endpoints)
		st.switched = now
		a, err := udpAddr(st.peer.Endpoints[st.idx])
		if err != nil {
			continue
		}
		pcs = append(pcs, wgtypes.PeerConfig{PublicKey: dp.PublicKey, UpdateOnly: true, Endpoint: a})
	}
	if len(pcs) == 0 {
		return nil
	}
	return m.wg.ConfigureDevice(m.cfg.Interface, wgtypes.Config{Peers: pcs})
}

// PeerStatus is an observed peer.
type PeerStatus struct {
	PublicKey     string
	Endpoint      string
	LastHandshake time.Time
	RxBytes       int64
	TxBytes       int64
}

// Status returns the current peer statuses.
func (m *Manager) Status() ([]PeerStatus, error) {
	dev, err := m.wg.Device(m.cfg.Interface)
	if err != nil {
		return nil, err
	}
	var out []PeerStatus
	for _, p := range dev.Peers {
		ps := PeerStatus{PublicKey: p.PublicKey.String(), LastHandshake: p.LastHandshakeTime, RxBytes: p.ReceiveBytes, TxBytes: p.TransmitBytes}
		if p.Endpoint != nil {
			ps.Endpoint = p.Endpoint.String()
		}
		out = append(out, ps)
	}
	return out, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
