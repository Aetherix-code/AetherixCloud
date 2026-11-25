package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/scheduler"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SchedulerLease is the cluster-wide lease held by the node running the
// placement Controller.
const SchedulerLease = "scheduler"

// Controller assigns replicas to machines. Exactly one node runs it at a time
// (lease SchedulerLease); the lease fencing token is stamped on every replica
// it creates.
//
// NOTE (ownership): this is the MINIMAL deploy behaviour used by `ax deploy`:
// new-deployment replicas are started first and the old deployment's replicas
// are retired once enough new replicas are RUNNING (start-first, no
// health-gated rollback). The rolling-deploy worker (internal/deploy) will
// take over deployment status transitions, surge limits and rollback.
type Controller struct {
	Store    store.Store
	Leases   *lease.Manager
	Interval time.Duration
	TTL      time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
	// OnChange is invoked after a pass that changed replica rows (optional).
	OnChange func()
	// Rollout optionally takes over deployment transitions (default:
	// DefaultRollout). MachineView optionally adjusts the machine list
	// (default: DefaultMachineView).
	Rollout     RolloutDriver
	MachineView MachineViewFunc
}

// Run blocks, holding the scheduler lease whenever possible.
func (c *Controller) Run(ctx context.Context) {
	ttl := c.TTL
	if ttl == 0 {
		ttl = 15 * time.Second
	}
	iv := c.Interval
	if iv == 0 {
		iv = 3 * time.Second
	}
	c.Leases.Run(ctx, SchedulerLease, ttl, func(ctx context.Context, l *lease.Lease) {
		ch, _ := c.Store.Watch(ctx, store.TableServices)
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			if err := c.RunOnce(ctx, l.Fence); err != nil && ctx.Err() == nil && c.Logger != nil {
				c.Logger.Warn("placement pass failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-ch:
			}
		}
	})
}

func (c *Controller) rollout() RolloutDriver {
	if c.Rollout != nil {
		return c.Rollout
	}
	return DefaultRollout
}

func (c *Controller) machineView() MachineViewFunc {
	if c.MachineView != nil {
		return c.MachineView
	}
	return DefaultMachineView
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// RunOnce performs one placement pass with the given fencing token.
func (c *Controller) RunOnce(ctx context.Context, fence int64) error {
	machines, err := store.ListMachines(ctx, c.Store)
	if err != nil {
		return err
	}
	if mv := c.machineView(); mv != nil {
		machines = mv(ctx, c.Store, machines, c.now())
	}
	services, err := store.ListServices(ctx, c.Store, nil)
	if err != nil {
		return err
	}
	allReplicas, err := store.ListReplicas(ctx, c.Store, nil)
	if err != nil {
		return err
	}
	svcByID := map[string]*v1.Service{}
	for _, s := range services {
		svcByID[s.Id] = s
	}
	byService := map[string][]*v1.Replica{}
	for _, r := range allReplicas {
		byService[r.ServiceId] = append(byService[r.ServiceId], r)
	}
	changed := false
	var firstErr error

	// Retire replicas of deleted services and GC old stopped rows.
	for _, r := range allReplicas {
		if _, ok := svcByID[r.ServiceId]; !ok && r.DesiredRunning {
			r.DesiredRunning = false
			r.State = v1.ReplicaState_REPLICA_STATE_STOPPING
			if err := store.PutReplica(ctx, c.Store, r); err == nil {
				changed = true
			}
		}
		if !r.DesiredRunning && r.State == v1.ReplicaState_REPLICA_STATE_STOPPED &&
			r.UpdatedAt != nil && c.now().Sub(r.UpdatedAt.AsTime()) > time.Minute {
			_ = c.Store.Delete(ctx, store.TableReplicas, r.Id)
		}
	}

	for _, svc := range services {
		if svc.ActiveDeploymentId == "" {
			continue
		}
		dep, err := store.GetDeployment(ctx, c.Store, svc.ActiveDeploymentId)
		if err != nil {
			continue
		}
		if drv := c.rollout(); drv != nil {
			handled, ch, err := drv.Rollout(ctx, RolloutInput{Store: c.Store, Fence: fence, Now: c.now(),
				Service: svc, Deployment: dep, Machines: machines, Replicas: byService[svc.Id]})
			changed = changed || ch
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if handled {
				continue
			}
		}
		ch, err := c.placeService(ctx, fence, svc, dep, machines, byService[svc.Id])
		changed = changed || ch
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if changed && c.OnChange != nil {
		c.OnChange()
	}
	return firstErr
}

func (c *Controller) placeService(ctx context.Context, fence int64, svc *v1.Service, dep *v1.Deployment, machines []*v1.Machine, reps []*v1.Replica) (bool, error) {
	changed := false
	var current, old []*v1.Replica
	for _, r := range reps {
		if !r.DesiredRunning {
			continue
		}
		if r.DeploymentId == dep.Id {
			current = append(current, r)
		} else {
			old = append(old, r)
		}
	}
	plan := scheduler.Schedule(dep.Spec, machines, current)
	if drv := c.rollout(); drv != nil && len(plan.Remove) > 0 {
		ch, err := drv.Retire(ctx, RolloutInput{Store: c.Store, Fence: fence, Now: c.now(), Service: svc,
			Deployment: dep, Machines: machines, Replicas: reps}, plan.Remove, current)
		changed = changed || ch
		if err != nil {
			return changed, err
		}
	} else {
		for _, r := range plan.Remove {
			r.DesiredRunning = false
			r.State = v1.ReplicaState_REPLICA_STATE_STOPPING
			r.UpdatedAt = timestamppb.New(c.now())
			if err := store.PutReplica(ctx, c.Store, r); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	for _, p := range plan.Place {
		r := &v1.Replica{
			Id: ids.New(), ServiceId: svc.Id, DeploymentId: dep.Id, MachineId: p.MachineID,
			State: v1.ReplicaState_REPLICA_STATE_PENDING, Fence: fence, DesiredRunning: true,
			UpdatedAt: timestamppb.New(c.now()),
		}
		if err := store.PutReplica(ctx, c.Store, r); err != nil {
			return changed, err
		}
		changed = true
	}
	for _, r := range plan.Stranded {
		if r.State != v1.ReplicaState_REPLICA_STATE_STRANDED {
			r.State = v1.ReplicaState_REPLICA_STATE_STRANDED
			r.Message = "machine is down and the service is not reschedulable"
			_ = store.PutReplica(ctx, c.Store, r)
			changed = true
		}
	}

	// Deployment status + retiring the previous deployment.
	want := int(dep.Spec.Replicas)
	running := 0
	for _, r := range current {
		if r.State == v1.ReplicaState_REPLICA_STATE_RUNNING {
			running++
		}
	}
	if dep.Spec.Mode == v1.ServiceMode_SERVICE_MODE_GLOBAL {
		want = len(current) + len(plan.Place)
	}
	ready := len(plan.Place) == 0 && running >= want
	if ready && dep.Status != v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE {
		dep.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE
		dep.FinishedAt = timestamppb.New(c.now())
		if err := store.PutDeployment(ctx, c.Store, dep); err != nil {
			return changed, err
		}
		changed = true
	} else if !ready && dep.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_QUEUED {
		dep.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_DEPLOYING
		_ = store.PutDeployment(ctx, c.Store, dep)
	}
	if ready || want == 0 {
		oldDeps := map[string]bool{}
		for _, r := range old {
			r.DesiredRunning = false
			r.State = v1.ReplicaState_REPLICA_STATE_STOPPING
			r.UpdatedAt = timestamppb.New(c.now())
			if err := store.PutReplica(ctx, c.Store, r); err != nil {
				return changed, err
			}
			oldDeps[r.DeploymentId] = true
			changed = true
		}
		for id := range oldDeps {
			if od, err := store.GetDeployment(ctx, c.Store, id); err == nil &&
				od.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE {
				od.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED
				_ = store.PutDeployment(ctx, c.Store, od)
			}
		}
	}
	if plan.Unplaceable > 0 {
		return changed, fmt.Errorf("service %s: %d replicas unplaceable: %v", svc.Name, plan.Unplaceable, plan.Reasons)
	}
	return changed, nil
}
