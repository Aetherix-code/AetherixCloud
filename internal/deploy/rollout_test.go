package deploy

import (
	"context"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/reconciler"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type env struct {
	t   *testing.T
	st  store.Store
	clk *clock
	rt  map[string]*runtime.Fake
	rec map[string]*reconciler.Reconciler
	ctl *reconciler.Controller
	svc *v1.Service
}

func newEnv(t *testing.T, cfg RolloutConfig, machines ...string) *env {
	st := store.NewMemory()
	clk := &clock{t: time.Now()}
	e := &env{t: t, st: st, clk: clk, rt: map[string]*runtime.Fake{}, rec: map[string]*reconciler.Reconciler{}}
	ctx := context.Background()
	for _, m := range machines {
		require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: m, Name: m, State: v1.MachineState_MACHINE_STATE_UP,
			Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}}))
		f := runtime.NewFake()
		f.AutoHealthy = true
		e.rt[m] = f
		e.rec[m] = reconciler.New(reconciler.Config{Store: st, Runtime: f, MachineID: m, ClusterID: "c1", Now: clk.Now})
	}
	e.ctl = &reconciler.Controller{Store: st, Leases: lease.NewManager(st, machines[0]), Now: clk.Now, Rollout: NewRollout(cfg)}
	e.svc = &v1.Service{Id: "svc00001", ProjectId: "p", EnvironmentId: "e", Name: "api"}
	return e
}

func (e *env) start(spec *v1.ServiceSpec) *v1.Deployment {
	e.svc.Spec = spec
	require.NoError(e.t, store.PutService(context.Background(), e.st, e.svc))
	dep, err := Start(context.Background(), e.st, e.svc, StartOptions{})
	require.NoError(e.t, err)
	return dep
}

func (e *env) step() {
	ctx := context.Background()
	_ = e.ctl.RunOnce(ctx, 1)
	for _, r := range e.rec {
		_ = r.ReconcileOnce(ctx)
	}
	e.clk.Advance(time.Second)
}

// serving counts replicas ingress would route to.
func (e *env) serving() int {
	ctx := context.Background()
	reps, _ := store.ListReplicas(ctx, e.st, nil)
	n := 0
	for _, r := range reps {
		if r.DesiredRunning && r.State == v1.ReplicaState_REPLICA_STATE_RUNNING {
			if m, _ := store.GetJSON[map[string]any](ctx, e.st, store.TableSettings, "ingress/drain/"+r.Id); m == nil {
				n++
			}
		}
	}
	return n
}

func (e *env) status(id string) v1.DeploymentStatus {
	d, err := store.GetDeployment(context.Background(), e.st, id)
	require.NoError(e.t, err)
	return d.Status
}

func (e *env) containers() int {
	n := 0
	for _, f := range e.rt {
		for _, c := range f.Containers {
			if c.Running() {
				n++
			}
		}
	}
	return n
}

func (e *env) until(max int, cond func() bool) bool {
	for i := 0; i < max; i++ {
		e.step()
		if cond() {
			return true
		}
	}
	return false
}

func TestRollingStartFirstZeroDowntime(t *testing.T) {
	e := newEnv(t, RolloutConfig{DrainDelay: 3 * time.Second}, "m1", "m2")
	d1 := e.start(&v1.ServiceSpec{Image: "app:1", Replicas: 2, HealthCheck: &v1.HealthCheck{HttpPath: "/"}})
	require.True(t, e.until(30, func() bool { return e.status(d1.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
	require.Equal(t, 2, e.serving())

	d2 := e.start(&v1.ServiceSpec{Image: "app:2", Replicas: 2, HealthCheck: &v1.HealthCheck{HttpPath: "/"}})
	sawSurge := false
	done := e.until(60, func() bool {
		require.GreaterOrEqual(t, e.serving(), 2, "capacity dipped below desired during start-first rollout")
		if e.containers() > 2 {
			sawSurge = true
		}
		return e.status(d2.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE && e.containers() == 2
	})
	require.True(t, done)
	require.True(t, sawSurge, "start-first must run old and new side by side")
	require.Equal(t, v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED, e.status(d1.Id))
	for _, f := range e.rt {
		for _, c := range f.Containers {
			require.Equal(t, d2.Id, c.Labels[runtime.LabelDeployment])
		}
	}
	hist, err := History(context.Background(), e.st, e.svc.Id, 10)
	require.NoError(t, err)
	require.Len(t, hist, 2)
	require.Equal(t, d2.Id, hist[0].Deployment.Id)
	var types []string
	for _, ev := range hist[0].Events {
		types = append(types, ev.Type)
	}
	require.Equal(t, []string{"deployment.created", "deployment.deploying", "deployment.active"}, types)
}

func TestMaxSurgeLimitsParallelism(t *testing.T) {
	e := newEnv(t, RolloutConfig{DrainDelay: time.Second}, "m1", "m2", "m3")
	d1 := e.start(&v1.ServiceSpec{Image: "app:1", Replicas: 3})
	require.True(t, e.until(30, func() bool { return e.status(d1.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
	d2 := e.start(&v1.ServiceSpec{Image: "app:2", Replicas: 3, MaxSurge: 1})
	peak := 0
	require.True(t, e.until(80, func() bool {
		peak = max(peak, e.containers())
		return e.status(d2.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE && e.containers() == 3
	}))
	require.LessOrEqual(t, peak, 4)
	require.Greater(t, peak, 3)
}

func TestStopFirstNeverRunsBoth(t *testing.T) {
	e := newEnv(t, RolloutConfig{DrainDelay: time.Second}, "m1")
	vol := []*v1.VolumeMount{{Source: "data", Target: "/data"}}
	d1 := e.start(&v1.ServiceSpec{Image: "db:1", Replicas: 1, Volumes: vol})
	require.Equal(t, v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST, StrategyOf(e.svc.Spec))
	require.True(t, e.until(30, func() bool { return e.status(d1.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
	d2 := e.start(&v1.ServiceSpec{Image: "db:2", Replicas: 1, Volumes: vol})
	require.True(t, e.until(60, func() bool {
		require.LessOrEqual(t, e.containers(), 1, "stop-first ran old and new together")
		return e.status(d2.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE && e.serving() == 1
	}))
	require.Equal(t, v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED, e.status(d1.Id))
}

func TestAutomaticRollbackWhenImageFails(t *testing.T) {
	e := newEnv(t, RolloutConfig{DrainDelay: time.Second, FailureGrace: 5 * time.Second}, "m1", "m2")
	d1 := e.start(&v1.ServiceSpec{Image: "app:1", Replicas: 2})
	require.True(t, e.until(30, func() bool { return e.status(d1.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
	for _, f := range e.rt {
		f.FailPull = map[string]bool{"app:bad": true}
	}
	d2 := e.start(&v1.ServiceSpec{Image: "app:bad", Replicas: 2})
	require.True(t, e.until(60, func() bool { return e.status(d2.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ROLLED_BACK }))
	require.Equal(t, 2, e.serving(), "old replicas kept serving")
	svc, _ := store.GetService(context.Background(), e.st, e.svc.Id)
	require.Equal(t, d1.Id, svc.ActiveDeploymentId)
	require.Equal(t, "app:1", svc.Spec.Image)
	d, _ := store.GetDeployment(context.Background(), e.st, d2.Id)
	require.Contains(t, d.StatusMessage, "failed")
	require.Equal(t, v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE, e.status(d1.Id))

	// the broken replicas get cleaned up and the old ones stay
	require.True(t, e.until(30, func() bool {
		reps, _ := store.ListReplicas(context.Background(), e.st, nil)
		for _, r := range reps {
			if r.DeploymentId == d2.Id && r.DesiredRunning {
				return false
			}
		}
		return true
	}))
	require.Equal(t, 2, e.serving())
	hist, _ := History(context.Background(), e.st, e.svc.Id, 10)
	found := false
	for _, ev := range hist[0].Events {
		found = found || ev.Type == "deployment.rolled_back"
	}
	require.True(t, found)
}

func TestRollbackOnUnhealthyDeadline(t *testing.T) {
	e := newEnv(t, RolloutConfig{DrainDelay: time.Second, ProgressDeadline: 30 * time.Second}, "m1")
	d1 := e.start(&v1.ServiceSpec{Image: "app:1", Replicas: 1})
	require.True(t, e.until(30, func() bool { return e.status(d1.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
	e.rt["m1"].AutoHealthy = false // new containers never pass their health check
	d2 := e.start(&v1.ServiceSpec{Image: "app:2", Replicas: 1, HealthCheck: &v1.HealthCheck{HttpPath: "/"}})
	require.True(t, e.until(80, func() bool { return e.status(d2.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ROLLED_BACK }))
	d, _ := store.GetDeployment(context.Background(), e.st, d2.Id)
	require.Contains(t, d.StatusMessage, "did not become healthy")
	require.Equal(t, 1, e.serving())
}

func TestFirstDeployFailureMarksFailedAndStops(t *testing.T) {
	e := newEnv(t, RolloutConfig{FailureGrace: 3 * time.Second}, "m1")
	e.rt["m1"].FailPull = map[string]bool{"app:bad": true}
	d := e.start(&v1.ServiceSpec{Image: "app:bad", Replicas: 1})
	require.True(t, e.until(40, func() bool { return e.status(d.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_FAILED }))
	require.Zero(t, e.serving())
}

func TestDrainWaitsForIngressAck(t *testing.T) {
	e := newEnv(t, RolloutConfig{DrainDelay: time.Second, DrainTimeout: 15 * time.Second}, "m1")
	ctx := context.Background()
	d1 := e.start(&v1.ServiceSpec{Image: "app:1", Replicas: 1})
	require.True(t, e.until(30, func() bool { return e.status(d1.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
	// an ingress machine that never acknowledges
	require.NoError(t, store.PutMachine(ctx, e.st, &v1.Machine{Id: "ing", Name: "ing", State: v1.MachineState_MACHINE_STATE_UP,
		Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_INGRESS}}))
	d2 := e.start(&v1.ServiceSpec{Image: "app:2", Replicas: 1, MaxSurge: 1})
	oldStopped := func() bool {
		reps, _ := store.ListReplicas(ctx, e.st, map[string]string{"deployment_id": d1.Id})
		for _, r := range reps {
			if r.DesiredRunning {
				return false
			}
		}
		return true
	}
	start := e.clk.Now()
	require.True(t, e.until(60, oldStopped))
	require.GreaterOrEqual(t, e.clk.Now().Sub(start), 14*time.Second, "old replica stopped before the drain timeout without an ack")
	require.True(t, e.until(10, func() bool { return e.status(d2.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
}

func TestCancelInProgress(t *testing.T) {
	e := newEnv(t, RolloutConfig{DrainDelay: time.Second}, "m1")
	ctx := context.Background()
	d1 := e.start(&v1.ServiceSpec{Image: "app:1", Replicas: 1})
	require.True(t, e.until(30, func() bool { return e.status(d1.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
	e.rt["m1"].AutoHealthy = false
	d2 := e.start(&v1.ServiceSpec{Image: "app:2", Replicas: 1, HealthCheck: &v1.HealthCheck{HttpPath: "/"}})
	e.until(3, func() bool { return false })
	svc, _ := store.GetService(ctx, e.st, e.svc.Id)
	require.NoError(t, Cancel(ctx, e.st, svc, d2.Id))
	require.Equal(t, v1.DeploymentStatus_DEPLOYMENT_STATUS_ROLLED_BACK, e.status(d2.Id))
	svc, _ = store.GetService(ctx, e.st, e.svc.Id)
	require.Equal(t, d1.Id, svc.ActiveDeploymentId)
	require.Error(t, Cancel(ctx, e.st, svc, d1.Id), "finished deployments cannot be cancelled")
}

func TestGlobalAndSteadyStateUntouched(t *testing.T) {
	e := newEnv(t, RolloutConfig{DrainDelay: time.Second}, "m1", "m2")
	d1 := e.start(&v1.ServiceSpec{Image: "agent:1", Mode: v1.ServiceMode_SERVICE_MODE_GLOBAL})
	require.True(t, e.until(30, func() bool { return e.status(d1.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE }))
	require.Equal(t, 2, e.serving())
	// steady state: the driver defers to the default placement logic
	handled, _, err := e.ctl.Rollout.Rollout(context.Background(), reconciler.RolloutInput{Store: e.st, Now: e.clk.Now(),
		Service: e.svc, Deployment: mustDep(t, e, d1.Id)})
	require.NoError(t, err)
	require.False(t, handled)
	d2 := e.start(&v1.ServiceSpec{Image: "agent:2", Mode: v1.ServiceMode_SERVICE_MODE_GLOBAL})
	require.True(t, e.until(60, func() bool {
		return e.status(d2.Id) == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE && e.containers() == 2
	}))
}

func mustDep(t *testing.T, e *env, id string) *v1.Deployment {
	d, err := store.GetDeployment(context.Background(), e.st, id)
	require.NoError(t, err)
	return d
}
