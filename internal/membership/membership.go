// Package membership provides SWIM-style failure detection with
// hashicorp/memberlist. Each machine has a state: up, suspect or down.
//
// Gossip runs over the WireGuard mesh (bind to the machine's mesh IP). The
// local view is available through Snapshot/Subscribe; Syncer persists it to
// the replicated store (machines.state) — run it under a lease so only one
// node writes.
package membership

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/hashicorp/memberlist"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DefaultPort is the gossip port.
const DefaultPort = 7946

// State is the health of a machine.
type State int

const (
	StateUp State = iota + 1
	StateSuspect
	StateDown
)

func (s State) String() string {
	switch s {
	case StateUp:
		return "up"
	case StateSuspect:
		return "suspect"
	case StateDown:
		return "down"
	}
	return "unknown"
}

// Proto converts to the API enum.
func (s State) Proto() v1.MachineState {
	switch s {
	case StateUp:
		return v1.MachineState_MACHINE_STATE_UP
	case StateSuspect:
		return v1.MachineState_MACHINE_STATE_SUSPECT
	case StateDown:
		return v1.MachineState_MACHINE_STATE_DOWN
	}
	return v1.MachineState_MACHINE_STATE_UNSPECIFIED
}

// Member is one machine as seen by this node.
type Member struct {
	ID        string
	Addr      string
	State     State
	LastSeen  time.Time
	DownSince time.Time
}

// Change is a state transition.
type Change struct {
	Member Member
	Old    State // 0 if newly discovered
}

// Config configures a Membership.
type Config struct {
	MachineID     string
	BindAddr      string // mesh IP (or 127.0.0.1 in tests)
	BindPort      int    // 0 => DefaultPort
	AdvertiseAddr string
	// SecretKey (16/24/32 bytes) encrypts gossip; nil disables.
	SecretKey []byte
	// Interval between state polls (default 1s).
	PollInterval time.Duration
	// Fast makes failure detection quick (tests / dev clusters).
	Fast   bool
	Logger *slog.Logger
	Now    func() time.Time
}

// Membership wraps memberlist.
type Membership struct {
	cfg Config
	ml  *memberlist.Memberlist

	mu      sync.Mutex
	members map[string]*Member
	subs    []chan Change
	cancel  context.CancelFunc
}

// New starts memberlist and the state-tracking loop.
func New(cfg Config) (*Membership, error) {
	mc := memberlist.DefaultLANConfig()
	if cfg.Fast {
		mc = memberlist.DefaultLocalConfig()
		mc.ProbeInterval = 300 * time.Millisecond
		mc.ProbeTimeout = 150 * time.Millisecond
		mc.SuspicionMult = 2
	}
	mc.Name = cfg.MachineID
	mc.BindAddr = cfg.BindAddr
	mc.BindPort = cfg.BindPort
	// Always pin DefaultPort when unset. Fast mode used to leave BindPort at 0
	// (ephemeral), but joinPeers dials DefaultPort on every peer mesh IP — so
	// AX_DEV/fast-membership clusters never formed a gossip view and Syncer
	// flipped freshly joined machines back to DOWN.
	if mc.BindPort == 0 {
		mc.BindPort = DefaultPort
	}
	if cfg.AdvertiseAddr != "" {
		mc.AdvertiseAddr = cfg.AdvertiseAddr
		mc.AdvertisePort = mc.BindPort
	}
	mc.SecretKey = cfg.SecretKey
	mc.Logger = log.New(io.Discard, "", 0)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Second
		if cfg.Fast {
			cfg.PollInterval = 100 * time.Millisecond
		}
	}
	m := &Membership{cfg: cfg, members: map[string]*Member{}}
	mc.Events = nil
	ml, err := memberlist.Create(mc)
	if err != nil {
		return nil, fmt.Errorf("membership: %w", err)
	}
	m.ml = ml
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.refresh()
	go m.loop(ctx)
	return m, nil
}

// Port returns the bound gossip port.
func (m *Membership) Port() int { return int(m.ml.LocalNode().Port) }

// Addr returns "ip:port" of this node.
func (m *Membership) Addr() string {
	n := m.ml.LocalNode()
	return net.JoinHostPort(n.Addr.String(), strconv.Itoa(int(n.Port)))
}

// Join contacts existing members ("ip:port" or "ip").
func (m *Membership) Join(addrs []string) error {
	_, err := m.ml.Join(addrs)
	m.refresh()
	return err
}

// Leave announces a graceful departure and shuts down.
func (m *Membership) Leave(timeout time.Duration) error {
	m.cancel()
	err := m.ml.Leave(timeout)
	if e := m.ml.Shutdown(); err == nil {
		err = e
	}
	return err
}

// Shutdown stops without announcing (simulates a crash in tests).
func (m *Membership) Shutdown() error {
	m.cancel()
	return m.ml.Shutdown()
}

func (m *Membership) loop(ctx context.Context) {
	t := time.NewTicker(m.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.refresh()
		}
	}
}

// classify maps a memberlist node state to ours.
func classify(s memberlist.NodeStateType) State {
	switch s {
	case memberlist.StateAlive:
		return StateUp
	case memberlist.StateSuspect:
		return StateSuspect
	default:
		return StateDown
	}
}

func (m *Membership) refresh() {
	now := m.cfg.Now()
	seen := map[string]bool{}
	var changes []Change
	m.mu.Lock()
	for _, n := range m.ml.Members() {
		seen[n.Name] = true
		st := classify(n.State)
		changes = m.apply(changes, n.Name, net.JoinHostPort(n.Addr.String(), strconv.Itoa(int(n.Port))), st, now)
	}
	// previously known, no longer listed (dead / left): down
	for id, mem := range m.members {
		if !seen[id] && mem.State != StateDown {
			changes = m.apply(changes, id, mem.Addr, StateDown, now)
		}
	}
	subs := append([]chan Change(nil), m.subs...)
	m.mu.Unlock()
	for _, c := range changes {
		for _, s := range subs {
			select {
			case s <- c:
			default:
			}
		}
	}
}

func (m *Membership) apply(changes []Change, id, addr string, st State, now time.Time) []Change {
	cur, ok := m.members[id]
	if !ok {
		cur = &Member{ID: id}
		m.members[id] = cur
	}
	old := cur.State
	cur.Addr = addr
	if st == StateUp {
		cur.LastSeen = now
		cur.DownSince = time.Time{}
	}
	if st == StateDown && old != StateDown {
		cur.DownSince = now
	}
	if old != st {
		cur.State = st
		changes = append(changes, Change{Member: *cur, Old: old})
	}
	return changes
}

// Snapshot returns all known members sorted by ID.
func (m *Membership) Snapshot() []Member {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Member, 0, len(m.members))
	for _, mem := range m.members {
		out = append(out, *mem)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// State returns the state of id (StateDown if unknown).
func (m *Membership) State(id string) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mem, ok := m.members[id]; ok {
		return mem.State
	}
	return StateDown
}

// Subscribe returns a channel of state transitions (buffered; slow consumers
// may miss events, re-read Snapshot).
func (m *Membership) Subscribe() <-chan Change {
	ch := make(chan Change, 64)
	m.mu.Lock()
	m.subs = append(m.subs, ch)
	m.mu.Unlock()
	return ch
}

// Syncer persists membership state into machines.state / last_seen and emits
// "machine.state" events. Run it under a lease so one node writes.
type Syncer struct {
	Mem    *Membership
	Store  store.Store
	Logger *slog.Logger
	// DownAfter delays marking a machine DOWN in the store (grace to avoid
	// flapping); 0 = immediately.
	Interval time.Duration
}

// Run blocks until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	iv := s.Interval
	if iv == 0 {
		iv = 2 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		_ = s.SyncOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SyncOnce writes changed states.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	machines, err := store.ListMachines(ctx, s.Store)
	if err != nil {
		return err
	}
	view := map[string]Member{}
	for _, m := range s.Mem.Snapshot() {
		view[m.ID] = m
	}
	for _, mc := range machines {
		mem, ok := view[mc.Id]
		st := v1.MachineState_MACHINE_STATE_DOWN
		if ok {
			st = mem.State.Proto()
		}
		stale := ok && mem.State == StateUp && (mc.LastSeen == nil || time.Since(mc.LastSeen.AsTime()) > 30*time.Second)
		if mc.State == st && !stale {
			continue
		}
		old := mc.State
		mc.State = st
		if ok && mem.State == StateUp {
			mc.LastSeen = timestamppb.New(mem.LastSeen)
		}
		if err := store.PutMachine(ctx, s.Store, mc); err != nil {
			return err
		}
		if old != st {
			_ = store.PutEvent(ctx, s.Store, &v1.Event{
				Id: fmt.Sprintf("%020d-%s", time.Now().UnixNano(), mc.Id), Type: "machine.state", SubjectId: mc.Id,
				Message:    fmt.Sprintf("machine %s is now %s", mc.Name, mem.State),
				Attributes: map[string]string{"state": st.String()}, Time: timestamppb.Now(),
			})
		}
	}
	return nil
}
