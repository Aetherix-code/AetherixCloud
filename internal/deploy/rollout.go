package deploy

import (
	"context"
	"fmt"
	"sort"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/ingress"
	"github.com/aetherix/aetherixcloud/internal/reconciler"
	"github.com/aetherix/aetherixcloud/internal/scheduler"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RolloutConfig tunes the rolling-deploy engine. Zero values pick defaults.
type RolloutConfig struct {
	// DrainDelay is the minimum time between taking a replica out of ingress
	// and stopping it, so in-flight requests finish (default 5s).
	DrainDelay time.Duration
	// DrainTimeout bounds the wait for every ingress node to acknowledge the
	// drain (default 20s); after it the replica is stopped regardless.
	DrainTimeout time.Duration
	// FailureGrace is how long a FAILED new replica is tolerated before the
	// rollout is rolled back (default 20s).
	FailureGrace time.Duration
	// ProgressDeadline is the maximum duration of a rollout (default 5m).
	ProgressDeadline time.Duration
	// MaxRestarts rolls back when a new replica restarted this often (default 3).
	MaxRestarts int32
	// MinReady is how long a replica must have been RUNNING (health passing)
	// before it counts as available (default 0).
	MinReady time.Duration
}

func (c *RolloutConfig) defaults() {
	if c.DrainDelay == 0 {
		c.DrainDelay = 5 * time.Second
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = 20 * time.Second
	}
	if c.FailureGrace == 0 {
		c.FailureGrace = 20 * time.Second
	}
	if c.ProgressDeadline == 0 {
		c.ProgressDeadline = 5 * time.Minute
	}
	if c.MaxRestarts == 0 {
		c.MaxRestarts = 3
	}
}

// Rollout is the health-gated rolling-deploy engine. It implements
// reconciler.RolloutDriver and runs inside the placement Controller (so under
// the "scheduler" lease, with the lease fencing token).
//
// Per service, per pass:
//
//  1. Failure detection (rollouts only): a new replica that stays FAILED,
//     restarts too often, or no progress before ProgressDeadline => automatic
//     rollback to the previous deployment (ROLLED_BACK + event).
//  2. Place missing replicas of the new deployment through the scheduler,
//     capped by the strategy: start-first allows replicas+maxSurge (min 1)
//     live replicas; stop-first allows at most `replicas`, so old replicas
//     must be fully stopped first (exclusive volumes).
//  3. Retire old replicas only as new ones become healthy: running old
//     replicas are first DRAINED (ingress stops routing to them and
//     acknowledges, see ingress.DrainApplied) and stopped after DrainDelay;
//     replicas that never served (not RUNNING) are stopped immediately.
//  4. When enough new replicas are healthy the deployment becomes ACTIVE and
//     the old deployments SUPERSEDED.
type Rollout struct{ cfg RolloutConfig }

// NewRollout returns the engine.
func NewRollout(cfg RolloutConfig) *Rollout {
	cfg.defaults()
	return &Rollout{cfg: cfg}
}

var _ reconciler.RolloutDriver = (*Rollout)(nil)

// StrategyOf returns the effective strategy: explicit, else stop-first when
// the service has volumes (exclusive), else start-first.
func StrategyOf(spec *v1.ServiceSpec) v1.DeployStrategy {
	switch spec.GetStrategy() {
	case v1.DeployStrategy_DEPLOY_STRATEGY_START_FIRST, v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST:
		return spec.Strategy
	}
	if len(spec.GetVolumes()) > 0 {
		return v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST
	}
	return v1.DeployStrategy_DEPLOY_STRATEGY_START_FIRST
}

type pass struct {
	ro      *Rollout
	in      reconciler.RolloutInput
	drains  map[string]ingress.DrainMark
	changed bool
}

func (ro *Rollout) Rollout(ctx context.Context, in reconciler.RolloutInput) (bool, bool, error) {
	dep := in.Deployment
	var cur, old []*v1.Replica
	for _, r := range in.Replicas {
		if !r.DesiredRunning {
			continue
		}
		if r.DeploymentId == dep.Id {
			cur = append(cur, r)
		} else {
			old = append(old, r)
		}
	}
	sort.Slice(old, func(i, j int) bool { return old[i].Id < old[j].Id })
	inProgress := dep.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_QUEUED || dep.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_DEPLOYING
	if !inProgress && len(old) == 0 {
		return false, false, nil
	}
	drains, err := ingress.Drains(ctx, in.Store)
	if err != nil {
		return true, false, err
	}
	p := &pass{ro: ro, in: in, drains: drains}
	err = p.run(ctx, cur, old, inProgress)
	return true, p.changed, err
}

func (p *pass) now() time.Time { return p.in.Now }

func (p *pass) ready(r *v1.Replica) bool {
	if r.State != v1.ReplicaState_REPLICA_STATE_RUNNING {
		return false
	}
	return p.ro.cfg.MinReady == 0 || r.UpdatedAt == nil || p.now().Sub(r.UpdatedAt.AsTime()) >= p.ro.cfg.MinReady
}

func (p *pass) run(ctx context.Context, cur, old []*v1.Replica, inProgress bool) error {
	in := p.in
	dep, svc := in.Deployment, in.Service
	st := in.Store

	if inProgress {
		if reason := p.failure(cur); reason != "" {
			return p.rollback(ctx, cur, old, reason)
		}
	}

	plan := scheduler.Schedule(dep.Spec, in.Machines, cur)
	removed := map[string]bool{}
	for _, r := range plan.Remove {
		removed[r.Id] = true
	}
	for _, r := range plan.Stranded {
		if r.State != v1.ReplicaState_REPLICA_STATE_STRANDED {
			r.State = v1.ReplicaState_REPLICA_STATE_STRANDED
			r.Message = "machine is down and the service is not reschedulable"
			if err := store.PutReplica(ctx, st, r); err != nil {
				return err
			}
			p.changed = true
		}
	}
	var live []*v1.Replica
	for _, r := range cur {
		if !removed[r.Id] {
			live = append(live, r)
		}
	}
	want := int(dep.Spec.Replicas)
	if dep.Spec.Mode == v1.ServiceMode_SERVICE_MODE_GLOBAL {
		want = len(live) + len(plan.Place)
	}

	// Surplus/ineligible replicas of the new deployment: retire gracefully.
	for _, r := range plan.Remove {
		if _, err := p.retire(ctx, r); err != nil {
			return err
		}
	}

	// Capacity left for new replicas under the strategy.
	stopFirst := StrategyOf(dep.Spec) == v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST
	surge := max(int(dep.Spec.MaxSurge), 1)
	holding := len(live)
	for _, r := range in.Replicas {
		if r.DeploymentId == dep.Id {
			continue
		}
		_, draining := p.drains[r.Id]
		switch {
		case r.DesiredRunning && !(draining && !stopFirst):
			holding++
		case !r.DesiredRunning && r.State == v1.ReplicaState_REPLICA_STATE_STOPPING:
			if stopFirst {
				holding++
			}
		}
	}
	limit := want + surge
	if stopFirst {
		limit = want
	}
	allowed := max(limit-holding, 0)
	place := plan.Place
	if len(place) > allowed {
		place = place[:allowed]
	}
	for _, pl := range place {
		r := &v1.Replica{Id: ids.New(), ServiceId: svc.Id, DeploymentId: dep.Id, MachineId: pl.MachineID,
			State: v1.ReplicaState_REPLICA_STATE_PENDING, Fence: in.Fence, DesiredRunning: true, UpdatedAt: timestamppb.New(p.now())}
		if err := store.PutReplica(ctx, st, r); err != nil {
			return err
		}
		p.changed = true
	}

	ready := 0
	for _, r := range live {
		if p.ready(r) {
			ready++
		}
	}
	complete := len(plan.Place) == 0 && ready >= want

	// Retire old replicas as availability allows.
	if err := p.retireOld(ctx, old, ready, want, inProgress, stopFirst); err != nil {
		return err
	}

	// Status transitions.
	switch {
	case inProgress && complete:
		dep.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE
		dep.StatusMessage = ""
		dep.FinishedAt = timestamppb.New(p.now())
		if err := store.PutDeployment(ctx, st, dep); err != nil {
			return err
		}
		p.changed = true
		p.event(ctx, "deployment.active", dep.Id, fmt.Sprintf("deployment of %s is active (%d/%d replicas healthy)", svc.Name, ready, want))
		p.supersede(ctx, old)
	case inProgress && dep.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_QUEUED:
		dep.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_DEPLOYING
		if err := store.PutDeployment(ctx, st, dep); err != nil {
			return err
		}
		p.changed = true
		p.event(ctx, "deployment.deploying", dep.Id, fmt.Sprintf("rolling out %s (%s)", svc.Name, strategyName(dep.Spec)))
	}
	if plan.Unplaceable > 0 && !complete {
		return fmt.Errorf("service %s: %d replicas unplaceable: %v", svc.Name, plan.Unplaceable, plan.Reasons)
	}
	return nil
}

func strategyName(spec *v1.ServiceSpec) string {
	if StrategyOf(spec) == v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST {
		return "stop-first"
	}
	return "start-first"
}

// failure returns a non-empty reason when the rollout must be rolled back.
func (p *pass) failure(cur []*v1.Replica) string {
	cfg, dep := p.ro.cfg, p.in.Deployment
	for _, r := range cur {
		if r.RestartCount >= cfg.MaxRestarts {
			return fmt.Sprintf("replica %s restarted %d times: %s", short(r.Id), r.RestartCount, r.Message)
		}
		if r.State == v1.ReplicaState_REPLICA_STATE_FAILED && r.UpdatedAt != nil &&
			p.now().Sub(r.UpdatedAt.AsTime()) >= cfg.FailureGrace {
			return fmt.Sprintf("replica %s failed: %s", short(r.Id), r.Message)
		}
	}
	if dep.CreatedAt != nil && p.now().Sub(dep.CreatedAt.AsTime()) > cfg.ProgressDeadline {
		return fmt.Sprintf("rollout did not become healthy within %s", cfg.ProgressDeadline)
	}
	return ""
}

func short(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}

// retireOld drains/stops old replicas according to availability.
func (p *pass) retireOld(ctx context.Context, old []*v1.Replica, ready, want int, inProgress, stopFirst bool) error {
	if len(old) == 0 {
		return nil
	}
	var serving []*v1.Replica // RUNNING, not draining
	for _, r := range old {
		if _, draining := p.drains[r.Id]; draining {
			// continue draining; stop when ripe
			if _, err := p.retire(ctx, r); err != nil {
				return err
			}
			continue
		}
		if r.State != v1.ReplicaState_REPLICA_STATE_RUNNING {
			if _, err := p.retire(ctx, r); err != nil { // never served: stop now
				return err
			}
			continue
		}
		serving = append(serving, r)
	}
	k := len(serving) // leftovers / scale to zero: retire everything
	if inProgress && want > 0 {
		minAvail := want
		if stopFirst {
			minAvail = want - max(int(p.in.Deployment.Spec.MaxSurge), 1)
		}
		k = min(max(ready+len(serving)-minAvail, 0), len(serving))
	}
	for _, r := range serving[:k] {
		if _, err := p.retire(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// Retire implements reconciler.RolloutDriver: graceful removal of replicas
// the Controller's steady-state placement wants gone.
func (ro *Rollout) Retire(ctx context.Context, in reconciler.RolloutInput, remove, current []*v1.Replica) (bool, error) {
	drains, err := ingress.Drains(ctx, in.Store)
	if err != nil {
		return false, err
	}
	p := &pass{ro: ro, in: in, drains: drains}
	gone := map[string]bool{}
	for _, r := range remove {
		gone[r.Id] = true
	}
	healthy := 0
	for _, r := range current {
		if !gone[r.Id] && p.ready(r) {
			healthy++
		}
	}
	global := in.Deployment.Spec.GetMode() == v1.ServiceMode_SERVICE_MODE_GLOBAL
	for _, r := range remove {
		if r.State == v1.ReplicaState_REPLICA_STATE_RUNNING && !global && healthy < int(in.Deployment.Spec.Replicas) {
			continue // keep serving until the replacement is healthy
		}
		if _, err := p.retire(ctx, r); err != nil {
			return p.changed, err
		}
	}
	return p.changed, nil
}

// retire stops r: immediately if it never served, otherwise after draining
// it from ingress. Returns whether the replica was stopped this call.
func (p *pass) retire(ctx context.Context, r *v1.Replica) (bool, error) {
	st := p.in.Store
	if r.State == v1.ReplicaState_REPLICA_STATE_RUNNING {
		mark, ok := p.drains[r.Id]
		if !ok {
			m, err := ingress.MarkDraining(ctx, st, r.Id, p.now())
			if err != nil {
				return false, err
			}
			p.drains[r.Id] = m
			p.changed = true
			return false, nil
		}
		elapsed := p.now().Sub(time.Unix(0, mark.At))
		if elapsed < p.ro.cfg.DrainDelay {
			return false, nil
		}
		if elapsed < p.ro.cfg.DrainTimeout {
			if acked, err := ingress.DrainApplied(ctx, st, mark); err != nil || !acked {
				return false, err
			}
		}
	}
	r.DesiredRunning = false
	r.State = v1.ReplicaState_REPLICA_STATE_STOPPING
	r.UpdatedAt = timestamppb.New(p.now())
	if err := store.PutReplica(ctx, st, r); err != nil {
		return false, err
	}
	_ = ingress.ClearDrain(ctx, st, r.Id)
	delete(p.drains, r.Id)
	p.changed = true
	return true, nil
}

// supersede marks the deployments of old replicas (and the previous
// deployment) as SUPERSEDED.
func (p *pass) supersede(ctx context.Context, old []*v1.Replica) {
	ids := map[string]bool{}
	if id := p.in.Deployment.PreviousDeploymentId; id != "" {
		ids[id] = true
	}
	for _, r := range old {
		ids[r.DeploymentId] = true
	}
	for id := range ids {
		if id == p.in.Deployment.Id {
			continue
		}
		if d, err := store.GetDeployment(ctx, p.in.Store, id); err == nil && d.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE {
			d.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED
			_ = store.PutDeployment(ctx, p.in.Store, d)
		}
	}
}

func (p *pass) event(ctx context.Context, typ, subject, msg string) {
	emit(ctx, p.in.Store, typ, subject, p.in.Service.Id, msg, p.now())
}

func emit(ctx context.Context, st store.Store, typ, deploymentID, serviceID, msg string, now time.Time) {
	_ = store.PutEvent(ctx, st, &v1.Event{Id: ids.New(), Type: typ, SubjectId: deploymentID, Message: msg,
		Time: timestamppb.New(now), Attributes: map[string]string{"service_id": serviceID}})
}

// rollback abandons the in-progress deployment and restores the previous one.
func (p *pass) rollback(ctx context.Context, cur, old []*v1.Replica, reason string) error {
	changed, err := rollbackTo(ctx, p.in.Store, p.in.Service, p.in.Deployment, cur, old, reason, p.now(), p.drains)
	p.changed = p.changed || changed
	return err
}

func rollbackTo(ctx context.Context, st store.Store, svc *v1.Service, dep *v1.Deployment, cur, old []*v1.Replica,
	reason string, now time.Time, drains map[string]ingress.DrainMark) (bool, error) {
	var prev *v1.Deployment
	if id := dep.PreviousDeploymentId; id != "" {
		if d, err := store.GetDeployment(ctx, st, id); err == nil && d.Spec != nil && d.Spec.Image != "" &&
			(d.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE || d.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED) {
			prev = d
		}
	}
	if prev == nil { // fall back to the newest deployment that still has replicas
		for _, r := range old {
			if d, err := store.GetDeployment(ctx, st, r.DeploymentId); err == nil && d.Spec != nil && d.Id != dep.Id &&
				(prev == nil || d.CreatedAt.AsTime().After(prev.CreatedAt.AsTime())) {
				prev = d
			}
		}
	}
	dep.FinishedAt = timestamppb.New(now)
	if prev == nil {
		// Nothing to go back to: stop the broken replicas so they do not crash-loop.
		dep.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_FAILED
		dep.StatusMessage = reason
		if err := store.PutDeployment(ctx, st, dep); err != nil {
			return false, err
		}
		for _, r := range cur {
			r.DesiredRunning, r.State, r.UpdatedAt = false, v1.ReplicaState_REPLICA_STATE_STOPPING, timestamppb.New(now)
			if err := store.PutReplica(ctx, st, r); err != nil {
				return true, err
			}
		}
		emit(ctx, st, "deployment.failed", dep.Id, svc.Id, fmt.Sprintf("deployment of %s failed: %s", svc.Name, reason), now)
		return true, nil
	}
	dep.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_ROLLED_BACK
	dep.StatusMessage = reason
	if err := store.PutDeployment(ctx, st, dep); err != nil {
		return false, err
	}
	if prev.Status != v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE {
		prev.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE
		if err := store.PutDeployment(ctx, st, prev); err != nil {
			return true, err
		}
	}
	svc.ActiveDeploymentId = prev.Id
	svc.Spec = proto.Clone(prev.Spec).(*v1.ServiceSpec)
	svc.UpdatedAt = timestamppb.New(now)
	if err := store.PutService(ctx, st, svc); err != nil {
		return true, err
	}
	// Replicas of the restored deployment that were being drained serve again.
	for _, r := range old {
		if r.DeploymentId == prev.Id {
			if _, ok := drains[r.Id]; ok {
				_ = ingress.ClearDrain(ctx, st, r.Id)
				delete(drains, r.Id)
			}
		}
	}
	emit(ctx, st, "deployment.rolled_back", dep.Id, svc.Id,
		fmt.Sprintf("deployment of %s rolled back to %s: %s", svc.Name, short(prev.Id), reason), now)
	// The failed deployment's replicas are now "old": the next pass drains
	// the ones that served and stops the rest.
	return true, nil
}

// Cancel aborts an in-progress deployment (queued/deploying), restoring the
// previous deployment. It is a no-op error for finished deployments.
func Cancel(ctx context.Context, st store.Store, svc *v1.Service, deploymentID string) error {
	dep, err := store.GetDeployment(ctx, st, deploymentID)
	if err != nil || dep.ServiceId != svc.Id {
		return fmt.Errorf("deployment %q not found for service %s", deploymentID, svc.Name)
	}
	if dep.Status != v1.DeploymentStatus_DEPLOYMENT_STATUS_QUEUED && dep.Status != v1.DeploymentStatus_DEPLOYMENT_STATUS_DEPLOYING &&
		dep.Status != v1.DeploymentStatus_DEPLOYMENT_STATUS_BUILDING {
		return fmt.Errorf("deployment %s is %s, not in progress", short(dep.Id), dep.Status)
	}
	reps, err := store.ListReplicas(ctx, st, map[string]string{"service_id": svc.Id})
	if err != nil {
		return err
	}
	var cur, old []*v1.Replica
	for _, r := range reps {
		if !r.DesiredRunning {
			continue
		}
		if r.DeploymentId == dep.Id {
			cur = append(cur, r)
		} else {
			old = append(old, r)
		}
	}
	drains, _ := ingress.Drains(ctx, st)
	if svc.ActiveDeploymentId != dep.Id { // never became active (e.g. still building)
		dep.Status, dep.StatusMessage, dep.FinishedAt = v1.DeploymentStatus_DEPLOYMENT_STATUS_FAILED, "cancelled", timestamppb.Now()
		return store.PutDeployment(ctx, st, dep)
	}
	_, err = rollbackTo(ctx, st, svc, dep, cur, old, "cancelled by user", time.Now(), drains)
	return err
}
