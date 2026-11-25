// Package reschedule handles machine failure and maintenance:
//
//   - Failure detection grace: a machine must stay DOWN for Config.Grace
//     (default 60s) before anything is moved. The first node to notice
//     records "down since" in the store.
//   - Lease-held rescheduling: the healthy machine with the lowest ID
//     claims the lease "reschedule:<machine>" and, for `reschedulable`
//     services, releases the lost replicas (desired_running=false, stamped
//     with the lease fencing token). The placement Controller
//     (internal/reconciler) then creates replacements through the scheduler,
//     honouring placement constraints, resources and spread. Before
//     releasing, the scheduler is simulated: if no machine can take the
//     replacement the replica is marked STRANDED instead of being dropped.
//   - Volume guard: replicas of services with local volumes are rescheduled
//     only when every volume is persistence="backup-restore"; otherwise they
//     stay assigned and are marked STRANDED with an explanation.
//   - Fencing: released rows carry desired_running=false and a new fence. The
//     recovered node's reconciler stops the stale container (and its
//     setState refuses to overwrite the row); rows later GC'd turn the
//     container into an orphan that is removed as well.
//   - Drain/cordon: the scheduler treats cordoned/draining machines as
//     ineligible and plans removal of their reschedulable replicas; the
//     Controller hands removals to deploy.Rollout.Retire, which waits for the
//     replacement to be healthy, drains the replica from ingress and then
//     stops it (graceful, no capacity dip).
package reschedule

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/ingress"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/reconciler"
	"github.com/aetherix/aetherixcloud/internal/scheduler"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const downSincePrefix = "reschedule/downsince/"

// Config tunes the Controller.
type Config struct {
	// Grace is how long a machine must be DOWN before its replicas are moved
	// (default 60s).
	Grace time.Duration
	// Interval between passes (default 5s).
	Interval time.Duration
	// LeaseTTL for reschedule:<machine> (default 30s).
	LeaseTTL time.Duration
	Now      func() time.Time
	Logger   *slog.Logger
}

func (c *Config) defaults() {
	if c.Grace == 0 {
		c.Grace = 60 * time.Second
	}
	if c.Interval == 0 {
		c.Interval = 5 * time.Second
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Controller runs on every node; at most one node acts per failed machine.
type Controller struct {
	Config
	Store  store.Store
	Leases *lease.Manager
	Self   string
}

// New returns a Controller.
func New(cfg Config, st store.Store, leases *lease.Manager, self string) *Controller {
	cfg.defaults()
	return &Controller{Config: cfg, Store: st, Leases: leases, Self: self}
}

// Run blocks until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	c.defaults()
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		if err := c.Pass(ctx); err != nil && ctx.Err() == nil {
			c.Logger.Warn("reschedule pass", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type downSince struct {
	Machine string `json:"machine"`
	Since   int64  `json:"since"`
}

func isUp(m *v1.Machine) bool {
	return m.State == v1.MachineState_MACHINE_STATE_UP || m.State == v1.MachineState_MACHINE_STATE_UNSPECIFIED
}

// responsible reports whether self is the healthy machine with the lowest ID
// other than target.
func responsible(machines []*v1.Machine, target, self string) bool {
	var ids []string
	for _, m := range machines {
		if m.Id != target && isUp(m) {
			ids = append(ids, m.Id)
		}
	}
	if len(ids) == 0 {
		return false
	}
	sort.Strings(ids)
	return ids[0] == self
}

// Pass performs one pass over all machines.
func (c *Controller) Pass(ctx context.Context) error {
	c.defaults()
	machines, err := store.ListMachines(ctx, c.Store)
	if err != nil {
		return err
	}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, m := range machines {
		switch {
		case m.State == v1.MachineState_MACHINE_STATE_DOWN:
			if responsible(machines, m.Id, c.Self) {
				note(c.handleDown(ctx, machines, m))
			}
		default:
			if _, err := c.Store.Get(ctx, store.TableSettings, downSincePrefix+m.Id); err == nil {
				_ = c.Store.Delete(ctx, store.TableSettings, downSincePrefix+m.Id)
			}
		}
	}
	return firstErr
}

// DownFor returns how long machine has been known to be down (0 if unknown).
func DownFor(ctx context.Context, st store.Store, machine string, now time.Time) time.Duration {
	d, err := store.GetJSON[downSince](ctx, st, store.TableSettings, downSincePrefix+machine)
	if err != nil {
		return 0
	}
	return now.Sub(time.Unix(0, d.Since))
}

func (c *Controller) replicasOn(ctx context.Context, machine string) ([]*v1.Replica, error) {
	reps, err := store.ListReplicas(ctx, c.Store, map[string]string{"machine_id": machine})
	if err != nil {
		return nil, err
	}
	var out []*v1.Replica
	for _, r := range reps {
		if r.DesiredRunning {
			out = append(out, r)
		}
	}
	return out, nil
}

// specOf returns the spec the replica was started with.
func (c *Controller) specOf(ctx context.Context, svc *v1.Service, r *v1.Replica, cache map[string]*v1.Deployment) *v1.ServiceSpec {
	d, ok := cache[r.DeploymentId]
	if !ok {
		d, _ = store.GetDeployment(ctx, c.Store, r.DeploymentId)
		cache[r.DeploymentId] = d
	}
	if d != nil && d.Spec != nil {
		return d.Spec
	}
	return svc.Spec
}

// VolumeGuard reports whether replicas of spec may be moved to another
// machine: every volume must use persistence "backup-restore".
func VolumeGuard(spec *v1.ServiceSpec) (bool, string) {
	for _, v := range spec.GetVolumes() {
		if v.Persistence != "backup-restore" {
			return false, fmt.Sprintf("volume %q is local to the machine and not set to backup-restore; "+
				"the replica stays stranded until the machine returns", v.Source)
		}
	}
	return true, ""
}

func (c *Controller) handleDown(ctx context.Context, machines []*v1.Machine, down *v1.Machine) error {
	now := c.Now()
	reps, err := c.replicasOn(ctx, down.Id)
	if err != nil {
		return err
	}
	ds, err := store.GetJSON[downSince](ctx, c.Store, store.TableSettings, downSincePrefix+down.Id)
	if store.IsNotFound(err) {
		return store.PutJSON(ctx, c.Store, store.TableSettings, downSincePrefix+down.Id, nil,
			downSince{Machine: down.Id, Since: now.UnixNano()})
	}
	if err != nil {
		return err
	}
	if now.Sub(time.Unix(0, ds.Since)) < c.Grace || len(reps) == 0 {
		return nil
	}
	l, err := c.Leases.Acquire(ctx, "reschedule:"+down.Id, c.LeaseTTL)
	if err != nil {
		return nil // someone else is on it
	}
	defer l.Release(context.WithoutCancel(ctx))

	deps := map[string]*v1.Deployment{}
	byService := map[string][]*v1.Replica{}
	for _, r := range reps {
		byService[r.ServiceId] = append(byService[r.ServiceId], r)
	}
	svcIDs := make([]string, 0, len(byService))
	for id := range byService {
		svcIDs = append(svcIDs, id)
	}
	sort.Strings(svcIDs)
	for _, sid := range svcIDs {
		lost := byService[sid]
		svc, err := store.GetService(ctx, c.Store, sid)
		if store.IsNotFound(err) {
			for _, r := range lost {
				c.release(ctx, l, r, "service deleted", now)
			}
			continue
		}
		if err != nil {
			return err
		}
		all, err := store.ListReplicas(ctx, c.Store, map[string]string{"service_id": sid})
		if err != nil {
			return err
		}
		var activeLive []*v1.Replica
		for _, r := range all {
			if r.DesiredRunning && r.DeploymentId == svc.ActiveDeploymentId {
				activeLive = append(activeLive, r)
			}
		}
		for _, r := range lost {
			spec := c.specOf(ctx, svc, r, deps)
			if !spec.GetReschedulable() {
				c.strand(ctx, r, fmt.Sprintf("machine %s is down and the service is not reschedulable", down.Name), now)
				continue
			}
			if ok, why := VolumeGuard(spec); !ok {
				c.strand(ctx, r, why, now)
				continue
			}
			if r.DeploymentId != svc.ActiveDeploymentId {
				c.release(ctx, l, r, "superseded replica on a down machine", now) // no replacement needed
				continue
			}
			sim := scheduler.Schedule(spec, machines, activeLive)
			if sim.Unplaceable > 0 {
				c.strand(ctx, r, fmt.Sprintf("no eligible machine to reschedule onto: %v", sim.Reasons), now)
				continue
			}
			c.release(ctx, l, r, fmt.Sprintf("rescheduled: machine %s was down for more than %s", down.Name, c.Grace), now)
			for _, v := range spec.Volumes {
				c.event(ctx, "volume.restore_requested", r.Id, svc.Id, fmt.Sprintf("replica of %s needs volume %q restored from its latest backup", svc.Name, v.Source),
					map[string]string{"volume": v.Source, "machine_id": down.Id})
			}
		}
	}
	return nil
}

func (c *Controller) strand(ctx context.Context, r *v1.Replica, why string, now time.Time) {
	if r.State == v1.ReplicaState_REPLICA_STATE_STRANDED && r.Message == why {
		return
	}
	n := proto.Clone(r).(*v1.Replica)
	n.State, n.Message, n.UpdatedAt = v1.ReplicaState_REPLICA_STATE_STRANDED, why, timestamppb.New(now)
	if err := store.PutReplica(ctx, c.Store, n); err == nil {
		c.event(ctx, "replica.stranded", r.Id, r.ServiceId, why, map[string]string{"machine_id": r.MachineId})
	}
}

// release hands a replica back: it is no longer desired, carries the lease
// fence and is marked STOPPED so the recovered node (or a GC) removes it.
func (c *Controller) release(ctx context.Context, l *lease.Lease, r *v1.Replica, why string, now time.Time) {
	n := proto.Clone(r).(*v1.Replica)
	n.DesiredRunning, n.State, n.Message = false, v1.ReplicaState_REPLICA_STATE_STOPPED, why
	n.Fence, n.UpdatedAt = l.Fence, timestamppb.New(now)
	if err := store.PutReplica(ctx, c.Store, n); err != nil {
		c.Logger.Warn("release replica", "replica", r.Id, "err", err)
		return
	}
	_ = ingress.ClearDrain(ctx, c.Store, r.Id)
	c.event(ctx, "replica.rescheduled", r.Id, r.ServiceId, why, map[string]string{"machine_id": r.MachineId})
}

func (c *Controller) event(ctx context.Context, typ, subject, serviceID, msg string, attrs map[string]string) {
	if attrs == nil {
		attrs = map[string]string{}
	}
	attrs["service_id"] = serviceID
	_ = store.PutEvent(ctx, c.Store, &v1.Event{Id: ids.New(), Type: typ, SubjectId: subject, Message: msg,
		Attributes: attrs, Time: timestamppb.New(c.Now())})
}

// MachineView is the reconciler.MachineViewFunc that keeps the placement
// Controller from reacting to a DOWN machine by itself: it sees SUSPECT
// instead, so replicas stay assigned (and no new ones land there) until the
// grace period elapsed and the reschedule lease holder released them.
func MachineView(_ context.Context, _ store.Store, machines []*v1.Machine, _ time.Time) []*v1.Machine {
	out := make([]*v1.Machine, len(machines))
	for i, m := range machines {
		if m.State == v1.MachineState_MACHINE_STATE_DOWN {
			m = proto.Clone(m).(*v1.Machine)
			m.State = v1.MachineState_MACHINE_STATE_SUSPECT
		}
		out[i] = m
	}
	return out
}

var _ reconciler.MachineViewFunc = MachineView
