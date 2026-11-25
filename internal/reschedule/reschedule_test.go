package reschedule

import (
	"context"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/deploy"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/reconciler"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

type env struct {
	t     *testing.T
	st    store.Store
	now   time.Time
	rt    map[string]*runtime.Fake
	rec   map[string]*reconciler.Reconciler
	ctl   *reconciler.Controller
	rs    map[string]*Controller // per node
	svc   *v1.Service
	alive map[string]bool
}

func newEnv(t *testing.T, machines ...*v1.Machine) *env {
	st := store.NewMemory()
	e := &env{t: t, st: st, now: time.Now(), rt: map[string]*runtime.Fake{}, rec: map[string]*reconciler.Reconciler{},
		rs: map[string]*Controller{}, alive: map[string]bool{}}
	ctx := context.Background()
	for _, m := range machines {
		m.State = v1.MachineState_MACHINE_STATE_UP
		require.NoError(t, store.PutMachine(ctx, st, m))
		f := runtime.NewFake()
		f.AutoHealthy = true
		e.rt[m.Id] = f
		e.alive[m.Id] = true
		e.rec[m.Id] = reconciler.New(reconciler.Config{Store: st, Runtime: f, MachineID: m.Id, ClusterID: "c", Now: e.clock})
		lm := &lease.Manager{Store: st, Self: m.Id, Now: e.clock}
		e.rs[m.Id] = New(Config{Grace: 60 * time.Second, Now: e.clock}, st, lm, m.Id)
	}
	e.ctl = &reconciler.Controller{Store: st, Leases: lease.NewManager(st, machines[0].Id), Now: e.clock,
		Rollout: deploy.NewRollout(deploy.RolloutConfig{DrainDelay: time.Second}), MachineView: MachineView}
	e.svc = &v1.Service{Id: "svc00001", ProjectId: "p", EnvironmentId: "e", Name: "api"}
	return e
}

func (e *env) clock() time.Time { return e.now }

func machine(id string) *v1.Machine {
	return &v1.Machine{Id: id, Name: id, Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}}
}

func (e *env) setState(id string, st v1.MachineState) {
	m, err := store.GetMachine(context.Background(), e.st, id)
	require.NoError(e.t, err)
	m.State = st
	require.NoError(e.t, store.PutMachine(context.Background(), e.st, m))
	e.alive[id] = st == v1.MachineState_MACHINE_STATE_UP
}

func (e *env) deploy(spec *v1.ServiceSpec) *v1.Deployment {
	e.svc.Spec = spec
	require.NoError(e.t, store.PutService(context.Background(), e.st, e.svc))
	d, err := deploy.Start(context.Background(), e.st, e.svc, deploy.StartOptions{})
	require.NoError(e.t, err)
	return d
}

// step: controller pass, reschedule passes on live nodes, reconcilers on live nodes.
func (e *env) step() {
	ctx := context.Background()
	_ = e.ctl.RunOnce(ctx, 1)
	for id, r := range e.rs {
		if e.alive[id] {
			require.NoError(e.t, r.Pass(ctx))
		}
	}
	for id, r := range e.rec {
		if e.alive[id] {
			_ = r.ReconcileOnce(ctx)
		}
	}
	e.now = e.now.Add(time.Second)
}

func (e *env) steps(n int) {
	for i := 0; i < n; i++ {
		e.step()
	}
}

func (e *env) replicas(f func(*v1.Replica) bool) []*v1.Replica {
	reps, _ := store.ListReplicas(context.Background(), e.st, nil)
	var out []*v1.Replica
	for _, r := range reps {
		if f(r) {
			out = append(out, r)
		}
	}
	return out
}

func desired(r *v1.Replica) bool { return r.DesiredRunning }
func on(m string) func(*v1.Replica) bool {
	return func(r *v1.Replica) bool { return r.DesiredRunning && r.MachineId == m }
}

func (e *env) events(typ string) []*v1.Event {
	evs, _ := store.ListProto[v1.Event](context.Background(), e.st, store.TableEvents, store.Query{})
	var out []*v1.Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env) deployed(spec *v1.ServiceSpec) {
	d := e.deploy(spec)
	e.steps(15)
	dd, _ := store.GetDeployment(context.Background(), e.st, d.Id)
	require.Equal(e.t, v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE, dd.Status)
}

func containersOf(f *runtime.Fake) int {
	n := 0
	for _, c := range f.Containers {
		if c.Running() {
			n++
		}
	}
	return n
}

func TestRescheduleAfterGraceWithFencing(t *testing.T) {
	e := newEnv(t, machine("m1"), machine("m2"), machine("m3"), machine("m4"))
	// pin the two replicas to m1,m2 first by cordoning the others
	for _, id := range []string{"m3", "m4"} {
		m, _ := store.GetMachine(context.Background(), e.st, id)
		m.Cordoned = true
		require.NoError(t, store.PutMachine(context.Background(), e.st, m))
	}
	e.deployed(&v1.ServiceSpec{Image: "app:1", Replicas: 2, Reschedulable: true})
	require.Len(t, e.replicas(on("m1")), 1)
	require.Len(t, e.replicas(on("m2")), 1)
	for _, id := range []string{"m3", "m4"} {
		m, _ := store.GetMachine(context.Background(), e.st, id)
		m.Cordoned = false
		require.NoError(t, store.PutMachine(context.Background(), e.st, m))
	}
	require.Equal(t, 1, containersOf(e.rt["m1"]))

	e.setState("m1", v1.MachineState_MACHINE_STATE_DOWN) // m1 dies with its container still "running"
	e.steps(30)                                          // < 60s grace
	require.Len(t, e.replicas(on("m1")), 1, "must wait for the grace period")
	require.Empty(t, e.events("replica.rescheduled"))
	require.Empty(t, e.replicas(on("m3")))

	e.steps(45) // past the grace period
	require.Empty(t, e.replicas(on("m1")))
	moved := append(e.replicas(on("m3")), e.replicas(on("m4"))...)
	require.Len(t, moved, 1, "replacement placed on a healthy machine")
	require.Len(t, e.replicas(desired), 2)
	require.Equal(t, v1.ReplicaState_REPLICA_STATE_RUNNING, moved[0].State)
	require.Len(t, e.events("replica.rescheduled"), 1)

	// fencing: the old row is fenced with the reschedule lease token
	old := e.replicas(func(r *v1.Replica) bool { return r.MachineId == "m1" })
	require.Len(t, old, 1)
	require.False(t, old[0].DesiredRunning)
	require.Equal(t, v1.ReplicaState_REPLICA_STATE_STOPPED, old[0].State)

	// m1 recovers: its reconciler must stop the stale container, not resurrect it
	require.Equal(t, 1, containersOf(e.rt["m1"]))
	e.setState("m1", v1.MachineState_MACHINE_STATE_UP)
	e.steps(3)
	require.Zero(t, containersOf(e.rt["m1"]), "stale container on the recovered node must be stopped")
	require.Len(t, e.replicas(desired), 2)
	require.Empty(t, e.replicas(on("m1")))
	// down-since bookkeeping cleaned up
	require.Zero(t, DownFor(context.Background(), e.st, "m1", e.now))
}

func TestOnlyLowestHealthyNodeActs(t *testing.T) {
	ms := []*v1.Machine{machine("m1"), machine("m2"), machine("m3")}
	require.True(t, responsible([]*v1.Machine{{Id: "m1", State: v1.MachineState_MACHINE_STATE_DOWN}, {Id: "m2", State: 1}, {Id: "m3", State: 1}}, "m1", "m2"))
	require.False(t, responsible([]*v1.Machine{{Id: "m1", State: 3}, {Id: "m2", State: 1}, {Id: "m3", State: 1}}, "m1", "m3"))
	// m2 itself is down => m3 takes over
	require.True(t, responsible([]*v1.Machine{{Id: "m1", State: 3}, {Id: "m2", State: 3}, {Id: "m3", State: 1}}, "m1", "m3"))
	_ = ms

	e := newEnv(t, machine("m1"), machine("m2"), machine("m3"))
	e.deployed(&v1.ServiceSpec{Image: "app:1", Replicas: 3, Reschedulable: true})
	e.setState("m1", v1.MachineState_MACHINE_STATE_DOWN)
	e.steps(5)
	e.now = e.now.Add(2 * time.Minute)
	// only m3 passes: it is not the lowest healthy node
	require.NoError(t, e.rs["m3"].Pass(context.Background()))
	require.Len(t, e.replicas(on("m1")), 1)
	require.NoError(t, e.rs["m2"].Pass(context.Background()))
	require.Empty(t, e.replicas(on("m1")))
}

func TestNotReschedulableIsStranded(t *testing.T) {
	e := newEnv(t, machine("m1"), machine("m2"))
	e.deployed(&v1.ServiceSpec{Image: "app:1", Replicas: 2})
	e.setState("m1", v1.MachineState_MACHINE_STATE_DOWN)
	e.steps(70)
	r := e.replicas(on("m1"))
	require.Len(t, r, 1)
	require.Equal(t, v1.ReplicaState_REPLICA_STATE_STRANDED, r[0].State)
	require.Contains(t, r[0].Message, "not reschedulable")
	require.Len(t, e.replicas(on("m2")), 1, "no replacement is created")
	require.Len(t, e.replicas(desired), 2)
	require.Len(t, e.events("replica.stranded"), 1)
}

func TestVolumeGuard(t *testing.T) {
	ok, why := VolumeGuard(&v1.ServiceSpec{Volumes: []*v1.VolumeMount{{Source: "data", Target: "/d", Persistence: "backup-restore"}}})
	require.True(t, ok)
	require.Empty(t, why)
	ok, why = VolumeGuard(&v1.ServiceSpec{Volumes: []*v1.VolumeMount{{Source: "data", Target: "/d"}}})
	require.False(t, ok)
	require.Contains(t, why, "backup-restore")
	ok, _ = VolumeGuard(&v1.ServiceSpec{})
	require.True(t, ok)

	// local volume => stranded even though reschedulable
	e := newEnv(t, machine("m1"), machine("m2"))
	e.deployed(&v1.ServiceSpec{Image: "db:1", Replicas: 1, Reschedulable: true, Volumes: []*v1.VolumeMount{{Source: "data", Target: "/d"}}})
	home := e.replicas(desired)[0].MachineId
	e.setState(home, v1.MachineState_MACHINE_STATE_DOWN)
	e.steps(70)
	r := e.replicas(on(home))
	require.Len(t, r, 1)
	require.Equal(t, v1.ReplicaState_REPLICA_STATE_STRANDED, r[0].State)
	require.Contains(t, r[0].Message, "backup-restore")
	require.Len(t, e.replicas(desired), 1)

	// backup-restore volume => rescheduled, restore requested
	e2 := newEnv(t, machine("m1"), machine("m2"))
	e2.deployed(&v1.ServiceSpec{Image: "db:1", Replicas: 1, Reschedulable: true, Volumes: []*v1.VolumeMount{{Source: "data", Target: "/d", Persistence: "backup-restore"}}})
	home = e2.replicas(desired)[0].MachineId
	e2.setState(home, v1.MachineState_MACHINE_STATE_DOWN)
	e2.steps(80)
	moved := e2.replicas(desired)
	require.Len(t, moved, 1)
	require.NotEqual(t, home, moved[0].MachineId)
	require.Len(t, e2.events("volume.restore_requested"), 1)
}

func TestHonoursPlacementAndResources(t *testing.T) {
	small := machine("m3")
	small.Capacity = &v1.Resources{CpuMillis: 100}
	e := newEnv(t, machine("m1"), machine("m2"), small)
	// placement restricted to m1, m2
	e.deployed(&v1.ServiceSpec{Image: "app:1", Replicas: 1, Reschedulable: true, Placement: &v1.Placement{Machines: []string{"m1", "m2"}},
		Resources: &v1.Resources{CpuMillis: 500}})
	home := e.replicas(desired)[0].MachineId
	other := map[string]string{"m1": "m2", "m2": "m1"}[home]

	// the only other allowed machine is cordoned => nowhere to go => stranded, not dropped
	m, _ := store.GetMachine(context.Background(), e.st, other)
	m.Cordoned = true
	require.NoError(t, store.PutMachine(context.Background(), e.st, m))
	e.setState(home, v1.MachineState_MACHINE_STATE_DOWN)
	e.steps(70)
	r := e.replicas(on(home))
	require.Len(t, r, 1)
	require.Equal(t, v1.ReplicaState_REPLICA_STATE_STRANDED, r[0].State)
	require.Contains(t, r[0].Message, "no eligible machine")
	require.Empty(t, e.replicas(on("m3")), "m3 violates placement and has too little cpu")

	// uncordon: it moves there (never to m3)
	m, _ = store.GetMachine(context.Background(), e.st, other)
	m.Cordoned = false
	require.NoError(t, store.PutMachine(context.Background(), e.st, m))
	e.steps(30)
	require.Len(t, e.replicas(on(other)), 1)
	require.Empty(t, e.replicas(on(home)))
	require.Empty(t, e.replicas(on("m3")))
}

func TestMachineBouncesWithinGraceKeepsReplicas(t *testing.T) {
	e := newEnv(t, machine("m1"), machine("m2"))
	e.deployed(&v1.ServiceSpec{Image: "app:1", Replicas: 2, Reschedulable: true})
	e.setState("m1", v1.MachineState_MACHINE_STATE_DOWN)
	e.steps(30)
	e.setState("m1", v1.MachineState_MACHINE_STATE_UP)
	e.steps(5)
	e.setState("m1", v1.MachineState_MACHINE_STATE_DOWN)
	e.steps(40) // the grace timer restarted: still < 60s
	require.Len(t, e.replicas(on("m1")), 1)
	require.Empty(t, e.events("replica.rescheduled"))
}

func TestDrainMovesReschedulableGracefully(t *testing.T) {
	e := newEnv(t, machine("m1"), machine("m2"), machine("m3"))
	e.deployed(&v1.ServiceSpec{Image: "app:1", Replicas: 2, Reschedulable: true})
	e.deployed2(&v1.ServiceSpec{Image: "pinned:1", Replicas: 1}) // not reschedulable
	victim := e.replicas(desired)[0].MachineId
	m, _ := store.GetMachine(context.Background(), e.st, victim)
	m.Cordoned, m.Draining = true, true
	require.NoError(t, store.PutMachine(context.Background(), e.st, m))

	minServing := 99
	for i := 0; i < 40; i++ {
		e.step()
		n := 0
		for _, r := range e.replicas(func(r *v1.Replica) bool {
			return r.DesiredRunning && r.State == v1.ReplicaState_REPLICA_STATE_RUNNING && r.ServiceId == e.svc.Id
		}) {
			if d, _ := store.GetJSON[map[string]any](context.Background(), e.st, store.TableSettings, "ingress/drain/"+r.Id); d == nil {
				n++
			}
		}
		minServing = min(minServing, n)
	}
	require.GreaterOrEqual(t, minServing, 2, "replacement must be healthy before the old replica is drained")
	for _, r := range e.replicas(on(victim)) {
		require.NotEqual(t, e.svc.Id, r.ServiceId, "reschedulable replicas left the draining machine")
	}
	require.Len(t, e.replicas(func(r *v1.Replica) bool { return r.DesiredRunning && r.ServiceId == e.svc.Id }), 2)
}

// deployed2 deploys a second service (pinned to the machine of the first replica).
func (e *env) deployed2(spec *v1.ServiceSpec) {
	svc := &v1.Service{Id: "svc00002", ProjectId: "p", EnvironmentId: "e", Name: "pinned", Spec: spec}
	victim := e.replicas(desired)[0].MachineId
	spec.Placement = &v1.Placement{Machines: []string{victim}}
	require.NoError(e.t, store.PutService(context.Background(), e.st, svc))
	_, err := deploy.Start(context.Background(), e.st, svc, deploy.StartOptions{})
	require.NoError(e.t, err)
	e.steps(10)
}

func TestMachineView(t *testing.T) {
	in := []*v1.Machine{
		{Id: "a", State: v1.MachineState_MACHINE_STATE_DOWN},
		{Id: "b", State: v1.MachineState_MACHINE_STATE_UP, Cordoned: true, Draining: true},
		{Id: "c", State: v1.MachineState_MACHINE_STATE_UP},
	}
	out := MachineView(context.Background(), nil, in, time.Now())
	require.Equal(t, v1.MachineState_MACHINE_STATE_SUSPECT, out[0].State)
	require.True(t, out[1].Draining, "draining machines are left to the scheduler/Retire")
	require.Equal(t, v1.MachineState_MACHINE_STATE_DOWN, in[0].State, "input is not mutated")
}
