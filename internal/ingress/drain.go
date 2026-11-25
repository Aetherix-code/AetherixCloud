package ingress

import (
	"context"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// DrainMark says a replica must receive no new requests from ingress.
type DrainMark struct {
	ReplicaID string `json:"replica_id"`
	// At is when draining was requested (unix nanoseconds).
	At int64 `json:"at"`
}

// MarkDraining requests that ingress stops routing to replicaID. It is
// idempotent: an existing mark is returned unchanged.
func MarkDraining(ctx context.Context, st store.Store, replicaID string, now time.Time) (DrainMark, error) {
	if m, err := GetDrain(ctx, st, replicaID); err == nil && m != nil {
		return *m, nil
	}
	m := DrainMark{ReplicaID: replicaID, At: now.UnixNano()}
	return m, putSetting(ctx, st, prefixDrain+replicaID, m)
}

// GetDrain returns the mark or nil.
func GetDrain(ctx context.Context, st store.Store, replicaID string) (*DrainMark, error) {
	m, err := store.GetJSON[DrainMark](ctx, st, store.TableSettings, prefixDrain+replicaID)
	if store.IsNotFound(err) {
		return nil, nil
	}
	return m, err
}

// ClearDrain removes the mark (replica gone).
func ClearDrain(ctx context.Context, st store.Store, replicaID string) error {
	return st.Delete(ctx, store.TableSettings, prefixDrain+replicaID)
}

// Drains returns all marks by replica ID.
func Drains(ctx context.Context, st store.Store) (map[string]DrainMark, error) {
	recs, err := listPrefix(ctx, st, prefixDrain)
	if err != nil {
		return nil, err
	}
	out := map[string]DrainMark{}
	for _, r := range recs {
		if m, err := decode[DrainMark](r); err == nil {
			out[m.ReplicaID] = m
		}
	}
	return out, nil
}

type appliedRow struct {
	Machine string `json:"machine"`
	// At is the time the store snapshot behind the applied config was read.
	At int64 `json:"at"`
}

func recordApplied(ctx context.Context, st store.Store, machine string, snapshot time.Time) error {
	return putSetting(ctx, st, prefixApplied+machine, appliedRow{Machine: machine, At: snapshot.UnixNano()})
}

// IsIngress reports whether m should run Caddy (explicit ingress role, or no
// roles recorded).
func IsIngress(m *v1.Machine) bool {
	if len(m.Roles) == 0 {
		return true
	}
	for _, r := range m.Roles {
		if r == v1.MachineRole_MACHINE_ROLE_INGRESS {
			return true
		}
	}
	return false
}

// DrainApplied reports whether every live ingress machine has applied a Caddy
// config built from a snapshot newer than the drain mark, i.e. no ingress
// node routes to the replica any more. With no live ingress machines it is
// trivially true.
func DrainApplied(ctx context.Context, st store.Store, mark DrainMark) (bool, error) {
	machines, err := store.ListMachines(ctx, st)
	if err != nil {
		return false, err
	}
	for _, m := range machines {
		if !IsIngress(m) || m.State == v1.MachineState_MACHINE_STATE_DOWN {
			continue
		}
		a, err := store.GetJSON[appliedRow](ctx, st, store.TableSettings, prefixApplied+m.Id)
		if store.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if a.At < mark.At {
			return false, nil
		}
	}
	return true, nil
}
