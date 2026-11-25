package registry

import (
	"context"
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

type pullRT struct {
	*runtime.Fake
	have map[string]bool
}

func (p *pullRT) PullImage(ctx context.Context, ref string) error {
	if !p.have[ref] {
		return runtime.ErrNotFound
	}
	return p.Fake.PullImage(ctx, ref)
}

func TestPeerPuller(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "a", MeshIp: "10.210.1.1"}))
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "b", MeshIp: "10.210.2.1", State: v1.MachineState_MACHINE_STATE_DOWN}))
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "c", MeshIp: "10.210.3.1"}))
	rt := &pullRT{Fake: runtime.NewFake(), have: map[string]bool{"10.210.3.1:5000/app:1": true, "docker.io/x": true}}
	p := PeerPuller{Store: st, Self: "a"}

	require.NoError(t, p.Pull(ctx, rt, "127.0.0.1:5000/app:1"))
	ok, _ := rt.ImageExists(ctx, "127.0.0.1:5000/app:1")
	require.True(t, ok)                                     // tagged under the original ref
	require.Empty(t, rt.CallsWithPrefix("pull:10.210.2.1")) // down peer skipped

	require.Error(t, p.Pull(ctx, rt, "127.0.0.1:5000/missing:1"))
	require.NoError(t, p.Pull(ctx, rt, "docker.io/x")) // non-local refs pull directly
}
