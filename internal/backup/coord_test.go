package backup

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

func newTestManager(st store.Store, self string) *Manager {
	m := NewManager(st, nil, self, nil)
	m.Leases = &lease.Manager{Store: st, Self: self, Settle: 0, Now: time.Now}
	return m
}

func putMachine(t *testing.T, st store.Store, id string, state v1.MachineState) {
	t.Helper()
	require.NoError(t, store.PutMachine(context.Background(), st, &v1.Machine{Id: id, Name: id, State: state}))
}

func putServiceWithVol(t *testing.T, st store.Store, id, vol, cron string) {
	t.Helper()
	require.NoError(t, store.PutService(context.Background(), st, &v1.Service{
		Id: id, Name: id, ProjectId: "p", EnvironmentId: "e",
		Spec: &v1.ServiceSpec{
			Image: "app", Replicas: 1,
			Volumes: []*v1.VolumeMount{{
				Source: vol, Target: "/data", Persistence: "backup-restore", BackupCron: cron,
			}},
		},
	}))
}

func putRunningReplica(t *testing.T, st store.Store, id, svc, machine string) {
	t.Helper()
	require.NoError(t, store.PutReplica(context.Background(), st, &v1.Replica{
		Id: id, ServiceId: svc, MachineId: machine,
		DesiredRunning: true, State: v1.ReplicaState_REPLICA_STATE_RUNNING,
	}))
}

func TestPreferredVolumeHost(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putRunningReplica(t, st, "r2", "svc", "m2")
	putRunningReplica(t, st, "r1", "svc", "m1")
	require.Equal(t, "m1", preferredVolumeHost(ctx, st, "svc"))
	require.Equal(t, "", preferredVolumeHost(ctx, st, "other"))
}

func TestShouldRunVolumeOnSelf_PreferHost(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putMachine(t, st, "m1", v1.MachineState_MACHINE_STATE_UP)
	putMachine(t, st, "m2", v1.MachineState_MACHINE_STATE_UP)
	putRunningReplica(t, st, "r1", "svc", "m1")

	m1 := newTestManager(st, "m1")
	m2 := newTestManager(st, "m2")
	require.True(t, m1.shouldRunVolumeOnSelf(ctx, "svc"))
	require.False(t, m2.shouldRunVolumeOnSelf(ctx, "svc"))
}

func TestShouldRunVolumeOnSelf_HostDownFallsBack(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putMachine(t, st, "m1", v1.MachineState_MACHINE_STATE_DOWN)
	putMachine(t, st, "m2", v1.MachineState_MACHINE_STATE_UP)
	putRunningReplica(t, st, "r1", "svc", "m1")

	m2 := newTestManager(st, "m2")
	require.True(t, m2.shouldRunVolumeOnSelf(ctx, "svc"))
}

func TestClusterBackupOnlyOneNodePerSlot(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	require.NoError(t, PutClusterSchedule(ctx, st, ClusterSchedule{
		Enabled: true, Cron: "* * * * *", Retain: 7, TargetID: "default",
	}))

	var calls atomic.Int32
	now := time.Date(2026, 6, 15, 4, 0, 0, 0, time.UTC)

	m1 := newTestManager(st, "m1")
	m2 := newTestManager(st, "m2")
	hook := func(context.Context) error { calls.Add(1); return nil }
	m1.testBackupCluster = hook
	m2.testBackupCluster = hook

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m1.backupClusterDue(ctx, now, map[string]string{}) }()
	go func() { defer wg.Done(); m2.backupClusterDue(ctx, now, map[string]string{}) }()
	wg.Wait()
	require.Equal(t, int32(1), calls.Load(), "exactly one cluster backup per cron slot")

	// Same slot again after leases released: store marker blocks a second run.
	m1.backupClusterDue(ctx, now, map[string]string{})
	m2.backupClusterDue(ctx, now, map[string]string{})
	require.Equal(t, int32(1), calls.Load())
}

func TestVolumeBackupPrefersHostAndLease(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putMachine(t, st, "m1", v1.MachineState_MACHINE_STATE_UP)
	putMachine(t, st, "m2", v1.MachineState_MACHINE_STATE_UP)
	putServiceWithVol(t, st, "svc", "data", "* * * * *")
	putRunningReplica(t, st, "r1", "svc", "m1")

	now := time.Date(2026, 6, 15, 3, 0, 0, 0, time.UTC)
	m1 := newTestManager(st, "m1")
	m2 := newTestManager(st, "m2")
	var ran1, ran2 atomic.Int32
	m1.testBackupVolume = func(context.Context, string, string) error { ran1.Add(1); return nil }
	m2.testBackupVolume = func(context.Context, string, string) error { ran2.Add(1); return nil }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m1.backupVolumesDue(ctx, now, map[string]string{}) }()
	go func() { defer wg.Done(); m2.backupVolumesDue(ctx, now, map[string]string{}) }()
	wg.Wait()

	require.Equal(t, int32(1), ran1.Load(), "host m1 should run")
	require.Equal(t, int32(0), ran2.Load(), "non-host m2 should skip")
}

func TestVolumeBackupNoHost_SingleLeaseWinner(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putServiceWithVol(t, st, "svc", "data", "* * * * *")

	var calls atomic.Int32
	now := time.Date(2026, 6, 15, 3, 0, 0, 0, time.UTC)
	m1 := newTestManager(st, "m1")
	m2 := newTestManager(st, "m2")
	hook := func(context.Context, string, string) error { calls.Add(1); return nil }
	m1.testBackupVolume = hook
	m2.testBackupVolume = hook

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m1.backupVolumesDue(ctx, now, map[string]string{}) }()
	go func() { defer wg.Done(); m2.backupVolumesDue(ctx, now, map[string]string{}) }()
	wg.Wait()
	require.Equal(t, int32(1), calls.Load())
}

func TestManualExclusiveConflictsWithHeldLease(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	m1 := newTestManager(st, "m1")
	m2 := newTestManager(st, "m2")

	l, err := m1.Leases.Acquire(ctx, leaseCluster, time.Minute)
	require.NoError(t, err)
	defer l.Release(ctx)

	m2.testBackupCluster = func(context.Context) error { t.Fatal("should not run"); return nil }
	err = m2.BackupClusterExclusive(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already in progress")
}

func TestSlotMarkerSurvivesLeaseRelease(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	require.NoError(t, markSlotFired(ctx, st, "cluster", "2026-06-15-04-00"))
	require.True(t, slotAlreadyFired(ctx, st, "cluster", "2026-06-15-04-00"))
	require.False(t, slotAlreadyFired(ctx, st, "cluster", "2026-06-15-04-01"))
}
