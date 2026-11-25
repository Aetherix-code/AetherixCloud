package membership

import (
	"context"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/hashicorp/memberlist"
	"github.com/stretchr/testify/require"
)

func TestClassify(t *testing.T) {
	require.Equal(t, StateUp, classify(memberlist.StateAlive))
	require.Equal(t, StateSuspect, classify(memberlist.StateSuspect))
	require.Equal(t, StateDown, classify(memberlist.StateDead))
	require.Equal(t, StateDown, classify(memberlist.StateLeft))
	require.Equal(t, v1.MachineState_MACHINE_STATE_SUSPECT, StateSuspect.Proto())
}

func TestApplyTransitions(t *testing.T) {
	m := &Membership{members: map[string]*Member{}}
	now := time.Unix(100, 0)
	ch := m.apply(nil, "a", "1.1.1.1:1", StateUp, now)
	require.Len(t, ch, 1)
	require.Equal(t, State(0), ch[0].Old)
	require.Empty(t, m.apply(nil, "a", "1.1.1.1:1", StateUp, now)) // no change
	ch = m.apply(nil, "a", "1.1.1.1:1", StateSuspect, now)
	require.Equal(t, StateUp, ch[0].Old)
	m.apply(nil, "a", "1.1.1.1:1", StateDown, now.Add(time.Second))
	require.Equal(t, now.Add(time.Second), m.members["a"].DownSince)
	m.apply(nil, "a", "1.1.1.1:1", StateUp, now.Add(2*time.Second))
	require.True(t, m.members["a"].DownSince.IsZero())
}

func TestSyncerWritesStateAndEvent(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "a", Name: "a"}))
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "b", Name: "b", State: v1.MachineState_MACHINE_STATE_UP}))
	mem := &Membership{members: map[string]*Member{}}
	mem.apply(nil, "a", "x", StateUp, time.Now())
	// b unknown to the local view => down
	s := &Syncer{Mem: mem, Store: st}
	require.NoError(t, s.SyncOnce(ctx))
	a, _ := store.GetMachine(ctx, st, "a")
	b, _ := store.GetMachine(ctx, st, "b")
	require.Equal(t, v1.MachineState_MACHINE_STATE_UP, a.State)
	require.NotNil(t, a.LastSeen)
	require.Equal(t, v1.MachineState_MACHINE_STATE_DOWN, b.State)
	evs, _ := store.ListProto[v1.Event](ctx, st, store.TableEvents, store.Query{})
	require.Len(t, evs, 2)
}

func TestTwoNodeGossip(t *testing.T) {
	// Explicit ports: both bind 127.0.0.1, so they cannot share DefaultPort.
	a, err := New(Config{MachineID: "a", BindAddr: "127.0.0.1", BindPort: 17946, Fast: true})
	if err != nil {
		t.Skipf("cannot bind loopback: %v", err)
	}
	defer a.Shutdown()
	b, err := New(Config{MachineID: "b", BindAddr: "127.0.0.1", BindPort: 17947, Fast: true})
	require.NoError(t, err)
	if err := b.Join([]string{a.Addr()}); err != nil {
		t.Skipf("cannot join over loopback: %v", err)
	}
	require.Eventually(t, func() bool { return a.State("b") == StateUp && b.State("a") == StateUp }, 5*time.Second, 50*time.Millisecond)
	require.NoError(t, b.Shutdown())
	require.Eventually(t, func() bool { return a.State("b") == StateDown }, 15*time.Second, 100*time.Millisecond)
}
