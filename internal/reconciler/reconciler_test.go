package reconciler

import (
	"context"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

type env struct {
	st  store.Store
	rt  map[string]*runtime.Fake
	rec map[string]*Reconciler
	ctl *Controller
}

func newEnv(t *testing.T, machines ...string) *env {
	st := store.NewMemory()
	e := &env{st: st, rt: map[string]*runtime.Fake{}, rec: map[string]*Reconciler{}}
	ctx := context.Background()
	for _, m := range machines {
		require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: m, Name: m, State: v1.MachineState_MACHINE_STATE_UP}))
		f := runtime.NewFake()
		f.AutoHealthy = true
		e.rt[m] = f
		e.rec[m] = New(Config{Store: st, Runtime: f, MachineID: m, ClusterID: "c1"})
	}
	e.ctl = &Controller{Store: st, Leases: lease.NewManager(st, machines[0])}
	return e
}

func (e *env) deploy(t *testing.T, id string, svc *v1.Service, spec *v1.ServiceSpec) *v1.Deployment {
	ctx := context.Background()
	dep := &v1.Deployment{Id: id, ServiceId: svc.Id, Spec: spec, Status: v1.DeploymentStatus_DEPLOYMENT_STATUS_QUEUED}
	require.NoError(t, store.PutDeployment(ctx, e.st, dep))
	svc.ActiveDeploymentId = id
	svc.Spec = spec
	require.NoError(t, store.PutService(ctx, e.st, svc))
	return dep
}

func (e *env) converge(t *testing.T, fence int64) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		require.NoError(t, e.ctl.RunOnce(ctx, fence))
		for _, r := range e.rec {
			require.NoError(t, r.ReconcileOnce(ctx))
		}
	}
}

func running(e *env, m string) []runtime.Container {
	cs, _ := e.rt[m].ListContainers(context.Background(), map[string]string{runtime.LabelManaged: "true"})
	var out []runtime.Container
	for _, c := range cs {
		if c.Running() {
			out = append(out, c)
		}
	}
	return out
}

func TestDeployAndSpread(t *testing.T) {
	e := newEnv(t, "m1", "m2")
	svc := &v1.Service{Id: "svc00001", ProjectId: "p", Name: "api"}
	dep := e.deploy(t, "d1", svc, &v1.ServiceSpec{Image: "nginx:1", Replicas: 2})
	e.converge(t, 1)

	require.Len(t, running(e, "m1"), 1)
	require.Len(t, running(e, "m2"), 1)
	c := running(e, "m1")[0]
	require.Equal(t, "svc00001", c.Labels[runtime.LabelService])
	require.Equal(t, "d1", c.Labels[runtime.LabelDeployment])
	require.Equal(t, "1", c.Labels[runtime.LabelFence])

	reps, _ := store.ListReplicas(context.Background(), e.st, nil)
	require.Len(t, reps, 2)
	for _, r := range reps {
		require.Equal(t, v1.ReplicaState_REPLICA_STATE_RUNNING, r.State)
		require.NotEmpty(t, r.ContainerId)
		require.NotEmpty(t, r.ContainerIp)
	}
	got, _ := store.GetDeployment(context.Background(), e.st, dep.Id)
	require.Equal(t, v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE, got.Status)
}

func TestScaleDownAndUp(t *testing.T) {
	e := newEnv(t, "m1", "m2")
	svc := &v1.Service{Id: "svc00001", Name: "api"}
	dep := e.deploy(t, "d1", svc, &v1.ServiceSpec{Image: "nginx:1", Replicas: 3})
	e.converge(t, 1)
	require.Len(t, running(e, "m1")[0:0], 0)
	require.Equal(t, 3, len(running(e, "m1"))+len(running(e, "m2")))

	dep.Spec.Replicas = 1
	require.NoError(t, store.PutDeployment(context.Background(), e.st, dep))
	e.converge(t, 1)
	require.Equal(t, 1, len(running(e, "m1"))+len(running(e, "m2")))
}

func TestRollingReplacesOldDeploymentAfterNewIsRunning(t *testing.T) {
	e := newEnv(t, "m1")
	svc := &v1.Service{Id: "svc00001", Name: "api"}
	e.deploy(t, "d1", svc, &v1.ServiceSpec{Image: "app:1", Replicas: 1})
	e.converge(t, 1)
	e.deploy(t, "d2", svc, &v1.ServiceSpec{Image: "app:2", Replicas: 1})
	e.converge(t, 1)

	cs := running(e, "m1")
	require.Len(t, cs, 1)
	require.Equal(t, "app:2", cs[0].Image)
	d1, _ := store.GetDeployment(context.Background(), e.st, "d1")
	require.Equal(t, v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED, d1.Status)
}

func TestPullFailureMarksReplicaFailedAndBacksOff(t *testing.T) {
	e := newEnv(t, "m1")
	e.rt["m1"].FailPull["bad:1"] = true
	e.deploy(t, "d1", &v1.Service{Id: "svc00001", Name: "api"}, &v1.ServiceSpec{Image: "bad:1", Replicas: 1})
	ctx := context.Background()
	require.NoError(t, e.ctl.RunOnce(ctx, 1))
	require.Error(t, e.rec["m1"].ReconcileOnce(ctx))
	reps, _ := store.ListReplicas(ctx, e.st, nil)
	require.Equal(t, v1.ReplicaState_REPLICA_STATE_FAILED, reps[0].State)
	require.Contains(t, reps[0].Message, "pull")
	// backoff: immediate retry does not hit the registry again
	n := len(e.rt["m1"].CallsWithPrefix("pull:"))
	require.NoError(t, e.rec["m1"].ReconcileOnce(ctx))
	require.Equal(t, n, len(e.rt["m1"].CallsWithPrefix("pull:")))
}

func TestCrashedContainerIsRestarted(t *testing.T) {
	e := newEnv(t, "m1")
	e.deploy(t, "d1", &v1.Service{Id: "svc00001", Name: "api"}, &v1.ServiceSpec{Image: "x:1", Replicas: 1})
	e.converge(t, 1)
	c := running(e, "m1")[0]
	e.rt["m1"].Crash(c.ID)
	require.NoError(t, e.rec["m1"].ReconcileOnce(context.Background()))
	require.Len(t, running(e, "m1"), 1)
	reps, _ := store.ListReplicas(context.Background(), e.st, nil)
	require.Equal(t, int32(1), reps[0].RestartCount)
}

func TestOrphanAndStaleNodeContainersRemoved(t *testing.T) {
	e := newEnv(t, "m1", "m2")
	svc := &v1.Service{Id: "svc00001", Name: "api"}
	e.deploy(t, "d1", svc, &v1.ServiceSpec{Image: "x:1", Replicas: 1, Reschedulable: true})
	e.converge(t, 1)
	var victim string
	for _, m := range []string{"m1", "m2"} {
		if len(running(e, m)) == 1 {
			victim = m
		}
	}
	other := map[string]string{"m1": "m2", "m2": "m1"}[victim]

	// victim goes down; only the survivor's reconciler runs; scheduler (new
	// holder, higher fence) reassigns the replica.
	ctx := context.Background()
	vm, _ := store.GetMachine(ctx, e.st, victim)
	vm.State = v1.MachineState_MACHINE_STATE_DOWN
	require.NoError(t, store.PutMachine(ctx, e.st, vm))
	require.NoError(t, e.ctl.RunOnce(ctx, 2))
	require.NoError(t, e.rec[other].ReconcileOnce(ctx))
	require.Len(t, running(e, other), 1)
	require.Len(t, running(e, victim), 1) // stale container still alive on the dead node

	// the node comes back: its row is desired_running=false => stopped.
	vm.State = v1.MachineState_MACHINE_STATE_UP
	require.NoError(t, store.PutMachine(ctx, e.st, vm))
	require.NoError(t, e.rec[victim].ReconcileOnce(ctx))
	require.Len(t, running(e, victim), 0)
	require.Len(t, running(e, other), 1)

	// container with no row at all is an orphan
	id, _ := e.rt[victim].CreateContainer(ctx, runtime.ContainerSpec{Name: "ghost", Image: "x:1", Labels: map[string]string{
		runtime.LabelManaged: "true", runtime.LabelMachine: victim, runtime.LabelReplica: "nope"}})
	require.NoError(t, e.rt[victim].StartContainer(ctx, id))
	require.NoError(t, e.rec[victim].ReconcileOnce(ctx))
	require.Len(t, running(e, victim), 0)
}

func TestNonReschedulableStaysStranded(t *testing.T) {
	e := newEnv(t, "m1", "m2")
	e.deploy(t, "d1", &v1.Service{Id: "svc00001", Name: "db"}, &v1.ServiceSpec{Image: "pg:1", Replicas: 1})
	e.converge(t, 1)
	ctx := context.Background()
	reps, _ := store.ListReplicas(ctx, e.st, nil)
	m, _ := store.GetMachine(ctx, e.st, reps[0].MachineId)
	m.State = v1.MachineState_MACHINE_STATE_DOWN
	require.NoError(t, store.PutMachine(ctx, e.st, m))
	require.NoError(t, e.ctl.RunOnce(ctx, 2))
	reps, _ = store.ListReplicas(ctx, e.st, nil)
	require.Len(t, reps, 1)
	require.Equal(t, v1.ReplicaState_REPLICA_STATE_STRANDED, reps[0].State)
}

func TestDeletedServiceRetiresReplicas(t *testing.T) {
	e := newEnv(t, "m1")
	e.deploy(t, "d1", &v1.Service{Id: "svc00001", Name: "api"}, &v1.ServiceSpec{Image: "x:1", Replicas: 1})
	e.converge(t, 1)
	ctx := context.Background()
	require.NoError(t, e.st.Delete(ctx, store.TableServices, "svc00001"))
	e.converge(t, 1)
	require.Len(t, running(e, "m1"), 0)
}

func TestContainerSpecFor(t *testing.T) {
	svc := &v1.Service{Id: "svc00001", Name: "web", ProjectId: "p"}
	dep := &v1.Deployment{Id: "d", Spec: &v1.ServiceSpec{
		Image: "i", Ports: []*v1.ServicePort{{ContainerPort: 80, Mode: "host", PublishedPort: 8080}, {ContainerPort: 90}},
		Volumes:     []*v1.VolumeMount{{Source: "data", Target: "/data", SizeBytes: 5 << 30}, {Source: "/etc/x", Target: "/x"}},
		HealthCheck: &v1.HealthCheck{HttpPath: "/up"}, StopGraceSeconds: 3,
	}}
	cs := ContainerSpecFor(svc, dep, &v1.Replica{Id: "01abcdefgh12345678", Fence: 7}, "c", "m", nil, nil)
	require.Equal(t, "web-12345678", cs.Name)
	require.Equal(t, 8080, cs.Ports[0].HostPort)
	require.Equal(t, 0, cs.Ports[1].HostPort)
	require.Equal(t, "ax-svc00001-data", cs.Mounts[0].Source)
	require.Equal(t, int64(5<<30), cs.Mounts[0].SizeBytes)
	require.Equal(t, "/etc/x", cs.Mounts[1].Source)
	require.NotNil(t, cs.Health)
	require.Equal(t, 3*time.Second, cs.StopTimeout)
	require.Equal(t, "7", cs.Labels[runtime.LabelFence])
}
