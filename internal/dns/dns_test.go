package dns

import (
	"context"
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	mdns "github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

type rw struct {
	mdns.ResponseWriter
	msg *mdns.Msg
}

func (r *rw) WriteMsg(m *mdns.Msg) error { r.msg = m; return nil }

func setup(t *testing.T) *Server {
	st := store.NewMemory()
	ctx := context.Background()
	require.NoError(t, store.PutProject(ctx, st, &v1.Project{Id: "p1", Name: "shop"}))
	require.NoError(t, store.PutProject(ctx, st, &v1.Project{Id: "p2", Name: "blog"}))
	require.NoError(t, store.PutService(ctx, st, &v1.Service{Id: "s1", ProjectId: "p1", Name: "api"}))
	require.NoError(t, store.PutService(ctx, st, &v1.Service{Id: "s2", ProjectId: "p2", Name: "api"}))
	rep := func(id, svc, ip string, st2 v1.ReplicaState) *v1.Replica {
		return &v1.Replica{Id: id, ServiceId: svc, ContainerIp: ip, State: st2, DesiredRunning: true}
	}
	for _, r := range []*v1.Replica{
		rep("r1", "s1", "10.210.1.130", v1.ReplicaState_REPLICA_STATE_RUNNING),
		rep("r2", "s1", "10.210.2.130", v1.ReplicaState_REPLICA_STATE_RUNNING),
		rep("r3", "s1", "10.210.3.130", v1.ReplicaState_REPLICA_STATE_STARTING),
		rep("r4", "s2", "10.210.1.131", v1.ReplicaState_REPLICA_STATE_RUNNING),
	} {
		require.NoError(t, store.PutReplica(ctx, st, r))
	}
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "m1", Name: "node-1", MeshIp: "10.210.1.1"}))
	return &Server{Store: st}
}

func TestResolve(t *testing.T) {
	s := setup(t)
	ctx := context.Background()

	ips, auth, err := s.Resolve(ctx, "api.shop.internal.")
	require.NoError(t, err)
	require.True(t, auth)
	require.Len(t, ips, 2) // only RUNNING replicas
	// round-robin: first answer alternates
	first := map[string]bool{}
	for i := 0; i < 4; i++ {
		ips, _, _ = s.Resolve(ctx, "api.shop.internal")
		first[ips[0].String()] = true
	}
	require.Len(t, first, 2)

	ips, _, _ = s.Resolve(ctx, "api.internal") // all projects
	require.Len(t, ips, 3)

	ips, _, _ = s.Resolve(ctx, "node-1.machine.internal")
	require.Equal(t, "10.210.1.1", ips[0].String())

	ips, auth, _ = s.Resolve(ctx, "nope.internal")
	require.Empty(t, ips)
	require.True(t, auth)

	_, auth, _ = s.Resolve(ctx, "example.com")
	require.False(t, auth)
}

func TestServeDNS(t *testing.T) {
	s := setup(t)
	q := new(mdns.Msg)
	q.SetQuestion("api.blog.internal.", mdns.TypeA)
	w := &rw{}
	s.ServeDNS(w, q)
	require.Equal(t, mdns.RcodeSuccess, w.msg.Rcode)
	require.Len(t, w.msg.Answer, 1)
	require.Equal(t, "10.210.1.131", w.msg.Answer[0].(*mdns.A).A.String())

	q.SetQuestion("ghost.internal.", mdns.TypeA)
	w = &rw{}
	s.ServeDNS(w, q)
	require.Equal(t, mdns.RcodeNameError, w.msg.Rcode)
}
