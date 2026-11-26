package backup

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/store"
)

const (
	firedPrefix    = "backup/fired/"
	leaseCluster   = "backup:cluster"
	leaseVolPrefix = "backup:vol:"
	// leaseTTL covers long tar+upload; renewed while the run is in progress.
	leaseTTL = 2 * time.Minute
)

// firedSlot records that a cron slot was claimed/completed cluster-wide so a
// failover mid-minute does not double-upload after the lease expires.
type firedSlot struct {
	Slot string    `json:"slot"`
	At   time.Time `json:"at"`
}

func volumeLeaseName(serviceID, volume string) string {
	return leaseVolPrefix + serviceID + ":" + volume
}

func firedID(fireKey string) string { return firedPrefix + fireKey }

func slotAlreadyFired(ctx context.Context, st store.Store, fireKey, slot string) bool {
	rec, err := store.GetJSON[firedSlot](ctx, st, store.TableSettings, firedID(fireKey))
	if err != nil || rec == nil {
		return false
	}
	return rec.Slot == slot
}

func markSlotFired(ctx context.Context, st store.Store, fireKey, slot string) error {
	return store.PutJSON(ctx, st, store.TableSettings, firedID(fireKey), nil, firedSlot{
		Slot: slot,
		At:   time.Now().UTC(),
	})
}

// preferredVolumeHost returns the lowest machine ID that currently hosts a
// running (or starting) replica of the service — that node has the local
// Docker volume. Empty means unknown / no live host.
func preferredVolumeHost(ctx context.Context, st store.Store, serviceID string) string {
	reps, err := store.ListReplicas(ctx, st, map[string]string{"service_id": serviceID})
	if err != nil || len(reps) == 0 {
		return ""
	}
	best := ""
	for _, r := range reps {
		if r.MachineId == "" || !r.DesiredRunning {
			continue
		}
		switch r.State {
		case v1.ReplicaState_REPLICA_STATE_RUNNING, v1.ReplicaState_REPLICA_STATE_STARTING:
			// ok
		default:
			continue
		}
		if best == "" || r.MachineId < best {
			best = r.MachineId
		}
	}
	return best
}

// shouldRunVolumeOnSelf reports whether this node should attempt a scheduled
// volume backup. Prefer the replica host; if that host is DOWN or unknown,
// any node may compete for the lease.
func (m *Manager) shouldRunVolumeOnSelf(ctx context.Context, serviceID string) bool {
	host := preferredVolumeHost(ctx, m.Store, serviceID)
	if host == "" || host == m.Self {
		return true
	}
	mach, err := store.GetMachine(ctx, m.Store, host)
	if err != nil {
		return true // unknown → compete via lease
	}
	if mach.State == v1.MachineState_MACHINE_STATE_DOWN {
		return true
	}
	return false
}

// holdLease runs fn while holding name. If Leases is nil (tests / single node
// without wiring), fn runs without a lease. ErrHeld is returned when another
// owner already holds the lease.
func (m *Manager) holdLease(ctx context.Context, name string, fn func(context.Context) error) error {
	if m.Leases == nil {
		return fn(ctx)
	}
	l, err := m.Leases.Acquire(ctx, name, leaseTTL)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release(context.WithoutCancel(ctx)) }()

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(leaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				if err := l.Renew(ctx); err != nil && (errors.Is(err, lease.ErrLost) || !l.Valid()) {
					cancel()
					return
				}
			}
		}
	}()
	err = fn(cctx)
	cancel()
	<-done
	return err
}

func isLeaseHeld(err error) bool {
	return err != nil && errors.Is(err, lease.ErrHeld)
}

// runExclusive claims fireKey for slot under a lease, then runs fn once.
// Returns (ran=false) when another node already claimed the slot or holds the lease.
func (m *Manager) runExclusive(ctx context.Context, leaseName, fireKey, slot string, fn func(context.Context) error) (ran bool, err error) {
	if slotAlreadyFired(ctx, m.Store, fireKey, slot) {
		return false, nil
	}
	err = m.holdLease(ctx, leaseName, func(ctx context.Context) error {
		if slotAlreadyFired(ctx, m.Store, fireKey, slot) {
			return nil
		}
		// Claim the slot before the upload so a mid-run failover does not
		// start a second upload of the same cron minute.
		if err := markSlotFired(ctx, m.Store, fireKey, slot); err != nil {
			return err
		}
		ran = true
		return fn(ctx)
	})
	if isLeaseHeld(err) {
		return false, nil
	}
	return ran, err
}

func (m *Manager) runVolumeBackup(ctx context.Context, serviceID, volume string) error {
	if m.testBackupVolume != nil {
		return m.testBackupVolume(ctx, serviceID, volume)
	}
	return m.BackupVolumeMount(ctx, serviceID, volume)
}

func (m *Manager) runClusterBackup(ctx context.Context) error {
	if m.testBackupCluster != nil {
		return m.testBackupCluster(ctx)
	}
	return m.BackupCluster(ctx)
}

// BackupVolumeMountExclusive is BackupVolumeMount under the volume lease so a
// manual run-now does not race a scheduled backup of the same volume.
func (m *Manager) BackupVolumeMountExclusive(ctx context.Context, serviceID, volume string) error {
	err := m.holdLease(ctx, volumeLeaseName(serviceID, volume), func(ctx context.Context) error {
		return m.BackupVolumeMount(ctx, serviceID, volume)
	})
	if isLeaseHeld(err) {
		return fmt.Errorf("backup already in progress on another node")
	}
	return err
}

// BackupClusterExclusive is BackupCluster under the cluster lease.
func (m *Manager) BackupClusterExclusive(ctx context.Context) error {
	err := m.holdLease(ctx, leaseCluster, func(ctx context.Context) error {
		return m.BackupCluster(ctx)
	})
	if isLeaseHeld(err) {
		return fmt.Errorf("backup already in progress on another node")
	}
	return err
}

// BackupAllVolumesNowExclusive snapshots every backup-restore volume under
// per-volume leases (skips volumes locked by another node).
func (m *Manager) BackupAllVolumesNowExclusive(ctx context.Context) {
	svcs, err := store.ListServices(ctx, m.Store, nil)
	if err != nil {
		return
	}
	for _, svc := range svcs {
		if svc.Spec == nil {
			continue
		}
		for _, v := range svc.Spec.Volumes {
			if v.Persistence != "backup-restore" {
				continue
			}
			if err := m.BackupVolumeMountExclusive(ctx, svc.Id, v.Source); err != nil {
				m.Logger.Warn("volume backup failed", "service", svc.Id, "volume", v.Source, "err", err)
			}
		}
	}
}
