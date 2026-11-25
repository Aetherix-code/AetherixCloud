// Package reconciler converges the containers running on THIS machine with the
// replica rows assigned to it in the replicated store (Reconciler), and places
// replicas for services across machines (Controller, run under the cluster
// scheduling lease).
//
// Fencing/stale-node safety: the Controller stamps every replica row with the
// lease fencing token. A machine that was partitioned or down and comes back
// sees that its rows have desired_running=false (reassigned) and stops the
// corresponding containers; it never starts containers for rows it does not
// own. Containers whose "aetherix.fence" label differs from the row's fence
// are recreated.
package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Puller makes an image available locally.
type Puller interface {
	Pull(ctx context.Context, rt runtime.Runtime, ref string) error
}

// DirectPuller pulls through the runtime.
type DirectPuller struct{}

func (DirectPuller) Pull(ctx context.Context, rt runtime.Runtime, ref string) error {
	return rt.PullImage(ctx, ref)
}

// EnvResolver produces the final environment for a replica (variable
// references, secrets). Owned by the deploy worker; default merges spec.env.
type EnvResolver func(ctx context.Context, svc *v1.Service, spec *v1.ServiceSpec) (map[string]string, error)

// Config configures a Reconciler.
type Config struct {
	Store      store.Store
	Runtime    runtime.Runtime
	Puller     Puller
	MachineID  string
	ClusterID  string
	DNSServers []string
	Env        EnvResolver
	Interval   time.Duration
	Logger     *slog.Logger
	// StopGrace is the default stop timeout.
	StopGrace time.Duration
	Now       func() time.Time
}

// Reconciler is the per-node loop.
type Reconciler struct {
	cfg Config

	mu          sync.Mutex
	lastRestart map[string]time.Time
	failCount   map[string]int
	trigger     chan struct{}
}

// New returns a Reconciler.
func New(cfg Config) *Reconciler {
	if cfg.Puller == nil {
		cfg.Puller = DirectPuller{}
	}
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Env == nil {
		cfg.Env = func(_ context.Context, _ *v1.Service, spec *v1.ServiceSpec) (map[string]string, error) {
			return spec.Env, nil
		}
	}
	return &Reconciler{cfg: cfg, lastRestart: map[string]time.Time{}, failCount: map[string]int{}, trigger: make(chan struct{}, 1)}
}

// Trigger requests an immediate reconcile.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run loops until ctx is done, reconciling on store changes and on a timer.
func (r *Reconciler) Run(ctx context.Context) error {
	ch, err := r.cfg.Store.Watch(ctx, store.TableReplicas)
	if err != nil {
		return err
	}
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		if err := r.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Logger.Warn("reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-r.trigger:
		case c, ok := <-ch:
			if !ok {
				return nil
			}
			// only react to rows for this machine
			if c.Op == store.OpPut && c.Record.Idx["machine_id"] != r.cfg.MachineID {
				continue
			}
		}
	}
}

// ReconcileOnce performs one convergence pass.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	cfg := r.cfg
	rows, err := store.ListReplicas(ctx, cfg.Store, map[string]string{"machine_id": cfg.MachineID})
	if err != nil {
		return err
	}
	containers, err := cfg.Runtime.ListContainers(ctx, map[string]string{
		runtime.LabelManaged: "true", runtime.LabelMachine: cfg.MachineID,
	})
	if err != nil {
		return err
	}
	byReplica := map[string]runtime.Container{}
	var orphans []runtime.Container
	rowByID := map[string]*v1.Replica{}
	for _, row := range rows {
		rowByID[row.Id] = row
	}
	for _, c := range containers {
		id := c.Labels[runtime.LabelReplica]
		if _, ok := rowByID[id]; !ok {
			orphans = append(orphans, c)
			continue
		}
		// keep the newest container if duplicates exist
		if prev, dup := byReplica[id]; dup {
			orphans = append(orphans, prev)
		}
		byReplica[id] = c
	}

	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// 1) Remove orphans: containers with no replica row on this machine
	//    (reassigned elsewhere, deleted, or stale after partition).
	for _, c := range orphans {
		cfg.Logger.Info("removing orphan container", "container", c.Name)
		note(r.removeContainer(ctx, c))
	}

	for _, row := range rows {
		c, have := byReplica[row.Id]
		if !row.DesiredRunning {
			if have {
				note(r.removeContainer(ctx, c))
			}
			r.setState(ctx, row, v1.ReplicaState_REPLICA_STATE_STOPPED, "", "", "")
			continue
		}
		if have && c.Labels[runtime.LabelFence] != strconv.FormatInt(row.Fence, 10) {
			// container belongs to an older placement of this replica row
			note(r.removeContainer(ctx, c))
			have = false
		}
		if !have {
			note(r.startReplica(ctx, row))
			continue
		}
		note(r.observe(ctx, row, c))
	}
	return firstErr
}

func (r *Reconciler) removeContainer(ctx context.Context, c runtime.Container) error {
	if c.Running() {
		if err := r.cfg.Runtime.StopContainer(ctx, c.ID, r.cfg.StopGrace); err != nil && err != runtime.ErrNotFound {
			r.cfg.Logger.Warn("stop failed", "container", c.Name, "err", err)
		}
	}
	if err := r.cfg.Runtime.RemoveContainer(ctx, c.ID, true); err != nil && err != runtime.ErrNotFound {
		return err
	}
	return nil
}

func (r *Reconciler) startReplica(ctx context.Context, row *v1.Replica) error {
	cfg := r.cfg
	svc, err := store.GetService(ctx, cfg.Store, row.ServiceId)
	if err != nil {
		if store.IsNotFound(err) {
			return nil // service deleted; controller will retire the row
		}
		return err
	}
	dep, err := store.GetDeployment(ctx, cfg.Store, row.DeploymentId)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	// Failure backoff.
	r.mu.Lock()
	n := r.failCount[row.Id]
	last := r.lastRestart[row.Id]
	r.mu.Unlock()
	if n > 0 && cfg.Now().Sub(last) < backoff(n) {
		return nil
	}
	fail := func(stage string, err error) error {
		r.mu.Lock()
		r.failCount[row.Id]++
		r.lastRestart[row.Id] = cfg.Now()
		r.mu.Unlock()
		r.setState(ctx, row, v1.ReplicaState_REPLICA_STATE_FAILED, "", "", fmt.Sprintf("%s: %v", stage, err))
		return fmt.Errorf("replica %s: %s: %w", row.Id, stage, err)
	}

	image := dep.Spec.Image
	ok, err := cfg.Runtime.ImageExists(ctx, image)
	if err != nil {
		return fail("inspect image", err)
	}
	if !ok {
		if err := cfg.Puller.Pull(ctx, cfg.Runtime, image); err != nil {
			return fail("pull", err)
		}
	}
	env, err := cfg.Env(ctx, svc, dep.Spec)
	if err != nil {
		return fail("resolve env", err)
	}
	spec := ContainerSpecFor(svc, dep, row, cfg.ClusterID, cfg.MachineID, env, cfg.DNSServers)
	id, err := cfg.Runtime.CreateContainer(ctx, spec)
	if err != nil {
		return fail("create", err)
	}
	if err := cfg.Runtime.StartContainer(ctx, id); err != nil {
		_ = cfg.Runtime.RemoveContainer(ctx, id, true)
		return fail("start", err)
	}
	c, err := cfg.Runtime.InspectContainer(ctx, id)
	if err != nil {
		return fail("inspect", err)
	}
	state := stateFor(dep.Spec, c)
	r.setState(ctx, row, state, id, c.IP, "")
	return nil
}

// observe syncs container state into the replica row and restarts crashed
// containers according to the restart policy.
func (r *Reconciler) observe(ctx context.Context, row *v1.Replica, c runtime.Container) error {
	cfg := r.cfg
	if c.Running() {
		dep, err := store.GetDeployment(ctx, cfg.Store, row.DeploymentId)
		if err != nil && !store.IsNotFound(err) {
			return err
		}
		var spec *v1.ServiceSpec
		if dep != nil {
			spec = dep.Spec
		}
		st := stateFor(spec, c)
		if st == v1.ReplicaState_REPLICA_STATE_RUNNING {
			r.mu.Lock()
			delete(r.failCount, row.Id)
			r.mu.Unlock()
		}
		msg := ""
		if c.Health == "unhealthy" {
			msg = "health check failing"
		}
		r.setState(ctx, row, st, c.ID, c.IP, msg)
		return nil
	}
	// Not running.
	policy := "always"
	if svc, err := store.GetDeployment(ctx, cfg.Store, row.DeploymentId); err == nil && svc.Spec.RestartPolicy != "" {
		policy = svc.Spec.RestartPolicy
	}
	if policy == "no" || (policy == "on-failure" && c.ExitCode == 0) {
		st := v1.ReplicaState_REPLICA_STATE_STOPPED
		if c.ExitCode != 0 {
			st = v1.ReplicaState_REPLICA_STATE_FAILED
		}
		r.setState(ctx, row, st, c.ID, "", fmt.Sprintf("exited with code %d", c.ExitCode))
		return nil
	}
	r.mu.Lock()
	n := r.failCount[row.Id]
	last := r.lastRestart[row.Id]
	r.mu.Unlock()
	if n > 0 && cfg.Now().Sub(last) < backoff(n) {
		r.setState(ctx, row, v1.ReplicaState_REPLICA_STATE_FAILED, c.ID, "", fmt.Sprintf("exited with code %d; restarting soon", c.ExitCode))
		return nil
	}
	r.mu.Lock()
	r.failCount[row.Id]++
	r.lastRestart[row.Id] = cfg.Now()
	r.mu.Unlock()
	row.RestartCount++
	if err := cfg.Runtime.StartContainer(ctx, c.ID); err != nil {
		r.setState(ctx, row, v1.ReplicaState_REPLICA_STATE_FAILED, c.ID, "", "restart: "+err.Error())
		return err
	}
	r.setState(ctx, row, v1.ReplicaState_REPLICA_STATE_STARTING, c.ID, c.IP, "restarted after exit")
	return nil
}

func backoff(n int) time.Duration {
	d := time.Second << min(n, 6)
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

func stateFor(spec *v1.ServiceSpec, c runtime.Container) v1.ReplicaState {
	if !c.Running() {
		return v1.ReplicaState_REPLICA_STATE_FAILED
	}
	if spec != nil && spec.HealthCheck != nil {
		switch c.Health {
		case "healthy":
			return v1.ReplicaState_REPLICA_STATE_RUNNING
		default:
			return v1.ReplicaState_REPLICA_STATE_STARTING
		}
	}
	return v1.ReplicaState_REPLICA_STATE_RUNNING
}

// setState writes the replica row only when something changed.
func (r *Reconciler) setState(ctx context.Context, row *v1.Replica, st v1.ReplicaState, containerID, ip, msg string) {
	next := proto.Clone(row).(*v1.Replica)
	next.State = st
	if containerID != "" || st == v1.ReplicaState_REPLICA_STATE_STOPPED {
		next.ContainerId = containerID
	}
	next.ContainerIp = ip
	next.Message = msg
	if proto.Equal(next, row) {
		return
	}
	next.UpdatedAt = timestamppb.New(r.cfg.Now())
	// Re-read to avoid clobbering controller changes (desired_running/fence).
	cur, err := store.GetReplica(ctx, r.cfg.Store, row.Id)
	if err != nil {
		return
	}
	if cur.DesiredRunning != row.DesiredRunning || cur.Fence != row.Fence {
		return // reassigned while we worked; next pass handles it
	}
	if err := store.PutReplica(ctx, r.cfg.Store, next); err != nil {
		r.cfg.Logger.Warn("update replica", "replica", row.Id, "err", err)
		return
	}
	proto.Reset(row)
	proto.Merge(row, next)
}
