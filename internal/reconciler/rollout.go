package reconciler

import (
	"context"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// RolloutInput is what the Controller hands to a RolloutDriver for one
// service whose active deployment is Deployment.
type RolloutInput struct {
	Store      store.Store
	Fence      int64
	Now        time.Time
	Service    *v1.Service
	Deployment *v1.Deployment
	// Machines is the (MachineView-adjusted) machine list.
	Machines []*v1.Machine
	// Replicas are ALL replica rows of the service (any deployment, any state).
	Replicas []*v1.Replica
}

// RolloutDriver takes over deployment transitions (health-gated rolling
// updates, drain, rollback) from the Controller's minimal built-in behaviour.
// It is implemented by internal/deploy.
type RolloutDriver interface {
	// Rollout is called once per pass per service. handled=false means the
	// Controller should run its default steady-state placement for the
	// service; handled=true means the driver did everything for this pass.
	Rollout(ctx context.Context, in RolloutInput) (handled, changed bool, err error)
	// Retire gracefully stops replicas the placement logic wants gone
	// (surplus after scale-down, a draining machine, a violated constraint):
	// the driver waits until the remaining replicas of the deployment are
	// healthy, takes the replica out of ingress, and only then stops it.
	// Replicas it leaves alone are offered again on the next pass. current
	// are all desired replicas of in.Deployment (including remove).
	Retire(ctx context.Context, in RolloutInput, remove, current []*v1.Replica) (changed bool, err error)
}

// MachineViewFunc adjusts the machine list the placement logic sees (e.g. to
// delay reacting to a machine that just went down, or to hand draining to a
// dedicated component).
type MachineViewFunc func(ctx context.Context, st store.Store, machines []*v1.Machine, now time.Time) []*v1.Machine

// DefaultRollout and DefaultMachineView are used by Controllers that do not
// set Rollout / MachineView themselves. Daemon hook files assign them before
// the daemon starts (see cmd/aetherixd/hooks_ingress.go).
var (
	DefaultRollout     RolloutDriver
	DefaultMachineView MachineViewFunc
)
