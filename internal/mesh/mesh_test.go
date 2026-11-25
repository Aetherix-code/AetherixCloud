package mesh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestAllocateSubnet(t *testing.T) {
	n := Network()
	p, err := AllocateSubnet(n, nil)
	require.NoError(t, err)
	require.Equal(t, "10.210.1.0/24", p.String())

	p, err = AllocateSubnet(n, []netip.Prefix{netip.MustParsePrefix("10.210.1.0/24"), netip.MustParsePrefix("10.210.3.0/24")})
	require.NoError(t, err)
	require.Equal(t, "10.210.2.0/24", p.String())

	var used []netip.Prefix
	for i := 1; i <= 254; i++ {
		pp, err := AllocateSubnet(n, used)
		require.NoError(t, err)
		used = append(used, pp)
	}
	_, err = AllocateSubnet(n, used)
	require.Error(t, err)

	_, err = AllocateSubnet(netip.MustParsePrefix("10.0.0.0/8"), nil)
	require.Error(t, err)
}

func TestAddressing(t *testing.T) {
	sn := netip.MustParsePrefix("10.210.3.0/24")
	require.Equal(t, "10.210.3.1", MeshIP(sn).String())
	require.Equal(t, "10.210.3.2", BridgeGateway(sn).String())
	require.Equal(t, "10.210.3.128/25", ContainerRange(sn).String())
	require.Equal(t, "10.210.3.1/16", InterfaceAddress(sn, netip.Prefix{}).String())
}

func TestKeyPair(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	k, err := wgtypes.ParseKey(kp.Private)
	require.NoError(t, err)
	require.Equal(t, kp.Public, k.PublicKey().String())
}

func TestPeersFromMachines(t *testing.T) {
	a, _ := GenerateKeyPair()
	b, _ := GenerateKeyPair()
	ms := []*v1.Machine{
		{Id: "m2", PublicKey: b.Public, Subnet: "10.210.2.0/24", Endpoints: []string{"1.2.3.4:51820"}},
		{Id: "m1", PublicKey: a.Public, Subnet: "10.210.1.0/24"},
		{Id: "m3", PublicKey: "", Subnet: "10.210.3.0/24"},
	}
	ps := PeersFromMachines("m1", ms)
	require.Len(t, ps, 1)
	require.Equal(t, "m2", ps[0].ID)
	require.Equal(t, "10.210.2.0/24", ps[0].AllowedIPs[0].String())
}

type fakeWG struct {
	dev  wgtypes.Device
	cfgs []wgtypes.Config
}

func (f *fakeWG) ConfigureDevice(_ string, c wgtypes.Config) error {
	f.cfgs = append(f.cfgs, c)
	for _, p := range c.Peers {
		if p.Remove {
			var keep []wgtypes.Peer
			for _, d := range f.dev.Peers {
				if d.PublicKey != p.PublicKey {
					keep = append(keep, d)
				}
			}
			f.dev.Peers = keep
			continue
		}
		found := false
		for i := range f.dev.Peers {
			if f.dev.Peers[i].PublicKey == p.PublicKey {
				found = true
				if p.Endpoint != nil {
					f.dev.Peers[i].Endpoint = p.Endpoint
				}
			}
		}
		if !found {
			f.dev.Peers = append(f.dev.Peers, wgtypes.Peer{PublicKey: p.PublicKey, Endpoint: p.Endpoint})
		}
	}
	return nil
}
func (f *fakeWG) Device(string) (*wgtypes.Device, error) { d := f.dev; return &d, nil }
func (f *fakeWG) Close() error                           { return nil }

type fakeLink struct{ ensured bool }

func (l *fakeLink) Ensure(context.Context, string, netip.Prefix, int) (string, error) {
	l.ensured = true
	return "kernel", nil
}
func (l *fakeLink) Delete(string) error { return nil }

func TestManagerUpSetPeersAndRotate(t *testing.T) {
	self, _ := GenerateKeyPair()
	p1, _ := GenerateKeyPair()
	p2, _ := GenerateKeyPair()
	fw, fl := &fakeWG{}, &fakeLink{}
	now := time.Unix(1000, 0)
	m := New(Config{PrivateKey: self.Private, Address: netip.MustParsePrefix("10.210.1.1/16"),
		Peers: []Peer{{ID: "m2", PublicKey: p1.Public, Endpoints: []string{"192.0.2.1:51820", "198.51.100.7:51820"},
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.210.2.0/24")}, Keepalive: 25 * time.Second}}}, fl, fw)
	m.Now = func() time.Time { return now }
	require.NoError(t, m.Up(context.Background()))
	require.True(t, fl.ensured)
	require.Equal(t, "kernel", m.Implementation())
	require.Len(t, fw.dev.Peers, 1)
	require.Equal(t, "192.0.2.1:51820", fw.dev.Peers[0].Endpoint.String())

	// no handshake for longer than RotateAfter => next endpoint
	now = now.Add(time.Minute)
	require.NoError(t, m.RotateStale())
	require.Equal(t, "198.51.100.7:51820", fw.dev.Peers[0].Endpoint.String())

	// a fresh handshake stops rotation
	now = now.Add(time.Minute)
	fw.dev.Peers[0].LastHandshakeTime = now.Add(-5 * time.Second)
	require.NoError(t, m.RotateStale())
	require.Equal(t, "198.51.100.7:51820", fw.dev.Peers[0].Endpoint.String())

	// replacing the peer set removes stale peers and adds new ones
	require.NoError(t, m.SetPeers(context.Background(), []Peer{{ID: "m3", PublicKey: p2.Public,
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.210.3.0/24")}}}))
	require.Len(t, fw.dev.Peers, 1)
	require.Equal(t, p2.Public, fw.dev.Peers[0].PublicKey.String())
}
