package backup

import (
	"context"
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

func TestPauseAndResumeService(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc := &v1.Service{
		Id: "svc1", Name: "app",
		Spec: &v1.ServiceSpec{Replicas: 2, Mode: v1.ServiceMode_SERVICE_MODE_REPLICATED},
	}
	require.NoError(t, store.PutService(ctx, st, svc))
	require.NoError(t, store.PutReplica(ctx, st, &v1.Replica{
		Id: "r1", ServiceId: "svc1", DesiredRunning: false, State: v1.ReplicaState_REPLICA_STATE_STOPPED,
	}))

	prev, err := pauseService(ctx, st, "svc1")
	require.NoError(t, err)
	require.Equal(t, int32(2), prev)

	got, err := store.GetService(ctx, st, "svc1")
	require.NoError(t, err)
	require.Equal(t, int32(0), got.Spec.Replicas)

	require.NoError(t, resumeService(ctx, st, "svc1", prev))
	got, err = store.GetService(ctx, st, "svc1")
	require.NoError(t, err)
	require.Equal(t, int32(2), got.Spec.Replicas)
}
