package api

import (
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/stretchr/testify/require"
)

func TestVisibleReplicas(t *testing.T) {
	running := &v1.Replica{Id: "run", DesiredRunning: true, State: v1.ReplicaState_REPLICA_STATE_RUNNING}
	pending := &v1.Replica{Id: "pend", DesiredRunning: true, State: v1.ReplicaState_REPLICA_STATE_PENDING}
	stopping := &v1.Replica{Id: "stopg", DesiredRunning: false, State: v1.ReplicaState_REPLICA_STATE_STOPPING}
	failed := &v1.Replica{Id: "fail", DesiredRunning: false, State: v1.ReplicaState_REPLICA_STATE_FAILED}
	retired := &v1.Replica{Id: "old", DesiredRunning: false, State: v1.ReplicaState_REPLICA_STATE_STOPPED}
	// Unusual but keep: still desired, marked stopped (e.g. mid-transition).
	desiredStopped := &v1.Replica{Id: "ds", DesiredRunning: true, State: v1.ReplicaState_REPLICA_STATE_STOPPED}

	got := visibleReplicas([]*v1.Replica{running, pending, stopping, failed, retired, desiredStopped})
	ids := make([]string, len(got))
	for i, r := range got {
		ids[i] = r.Id
	}
	require.Equal(t, []string{"run", "pend", "stopg", "fail", "ds"}, ids)

	// After stop then start: historical STOPPED + new RUNNING → only current.
	afterRestart := visibleReplicas([]*v1.Replica{
		{Id: "r1-old", DesiredRunning: false, State: v1.ReplicaState_REPLICA_STATE_STOPPED},
		{Id: "r2-old", DesiredRunning: false, State: v1.ReplicaState_REPLICA_STATE_STOPPED},
		{Id: "r1-new", DesiredRunning: true, State: v1.ReplicaState_REPLICA_STATE_RUNNING},
		{Id: "r2-new", DesiredRunning: true, State: v1.ReplicaState_REPLICA_STATE_RUNNING},
	})
	require.Len(t, afterRestart, 2)
	require.Equal(t, "r1-new", afterRestart[0].Id)
	require.Equal(t, "r2-new", afterRestart[1].Id)

	require.Empty(t, visibleReplicas(nil))
	require.Empty(t, visibleReplicas([]*v1.Replica{retired}))
}
