package scheduler

import (
	"fmt"
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/stretchr/testify/require"
)

func machines(n int) []*v1.Machine {
	var out []*v1.Machine
	for i := 1; i <= n; i++ {
		out = append(out, &v1.Machine{Id: fmt.Sprintf("m%d", i), Name: fmt.Sprintf("node-%d", i), State: v1.MachineState_MACHINE_STATE_UP})
	}
	return out
}

func placed(p Plan) map[string]int {
	c := map[string]int{}
	for _, x := range p.Place {
		c[x.MachineID]++
	}
	return c
}

func TestReplicatedSpread(t *testing.T) {
	p := Schedule(&v1.ServiceSpec{Replicas: 3}, machines(3), nil)
	require.Equal(t, map[string]int{"m1": 1, "m2": 1, "m3": 1}, placed(p))

	p = Schedule(&v1.ServiceSpec{Replicas: 5}, machines(2), nil)
	require.Equal(t, map[string]int{"m1": 3, "m2": 2}, placed(p))
}

func TestReplicatedScaleUpAccountsForExisting(t *testing.T) {
	ex := []*v1.Replica{{Id: "r1", MachineId: "m1"}, {Id: "r2", MachineId: "m1"}}
	p := Schedule(&v1.ServiceSpec{Replicas: 4}, machines(3), ex)
	require.Equal(t, map[string]int{"m2": 1, "m3": 1}, placed(p))
}

func TestScaleDownRemovesFromBusiestAndUnhealthyFirst(t *testing.T) {
	ex := []*v1.Replica{
		{Id: "r1", MachineId: "m1", State: v1.ReplicaState_REPLICA_STATE_RUNNING},
		{Id: "r2", MachineId: "m1", State: v1.ReplicaState_REPLICA_STATE_RUNNING},
		{Id: "r3", MachineId: "m2", State: v1.ReplicaState_REPLICA_STATE_FAILED},
	}
	p := Schedule(&v1.ServiceSpec{Replicas: 2}, machines(2), ex)
	require.Empty(t, p.Place)
	require.Len(t, p.Remove, 1)
	require.Equal(t, "r3", p.Remove[0].Id)

	p = Schedule(&v1.ServiceSpec{Replicas: 1}, machines(2), []*v1.Replica{
		{Id: "r1", MachineId: "m1", State: v1.ReplicaState_REPLICA_STATE_RUNNING},
		{Id: "r2", MachineId: "m1", State: v1.ReplicaState_REPLICA_STATE_RUNNING},
		{Id: "r3", MachineId: "m2", State: v1.ReplicaState_REPLICA_STATE_RUNNING},
	})
	require.Len(t, p.Remove, 2)
	require.Equal(t, "m1", p.Remove[0].MachineId) // busiest machine sheds first
}

func TestGlobal(t *testing.T) {
	ms := machines(3)
	ms[2].Cordoned = true
	ex := []*v1.Replica{{Id: "r1", MachineId: "m1"}}
	p := Schedule(&v1.ServiceSpec{Mode: v1.ServiceMode_SERVICE_MODE_GLOBAL}, ms, ex)
	require.Equal(t, map[string]int{"m2": 1}, placed(p))
	require.Empty(t, p.Remove)
}

func TestPlacementConstraints(t *testing.T) {
	ms := machines(3)
	ms[1].Labels = map[string]string{"disk": "ssd"}
	ms[2].Labels = map[string]string{"disk": "ssd"}
	spec := &v1.ServiceSpec{Replicas: 2, Placement: &v1.Placement{Labels: map[string]string{"disk": "ssd"}}}
	require.Equal(t, map[string]int{"m2": 1, "m3": 1}, placed(Schedule(spec, ms, nil)))

	spec = &v1.ServiceSpec{Replicas: 1, Placement: &v1.Placement{Machines: []string{"node-3"}}}
	require.Equal(t, map[string]int{"m3": 1}, placed(Schedule(spec, ms, nil)))

	// replica on a machine violating constraints is removed and replaced
	ex := []*v1.Replica{{Id: "r1", MachineId: "m1"}}
	p := Schedule(spec, ms, ex)
	require.Len(t, p.Remove, 1)
	require.Equal(t, map[string]int{"m3": 1}, placed(p))
}

func TestIneligibleStates(t *testing.T) {
	ms := machines(4)
	ms[0].State = v1.MachineState_MACHINE_STATE_DOWN
	ms[1].State = v1.MachineState_MACHINE_STATE_SUSPECT
	ms[2].Draining = true
	p := Schedule(&v1.ServiceSpec{Replicas: 2}, ms, nil)
	require.Equal(t, map[string]int{"m4": 2}, placed(p))

	p = Schedule(&v1.ServiceSpec{Replicas: 1}, nil, nil)
	require.Equal(t, 1, p.Unplaceable)
	require.NotEmpty(t, p.Reasons)
}

func TestRolesWorkerOnly(t *testing.T) {
	ms := machines(2)
	ms[0].Roles = []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_INGRESS}
	require.Equal(t, map[string]int{"m2": 1}, placed(Schedule(&v1.ServiceSpec{Replicas: 1}, ms, nil)))
}

func TestResourceFit(t *testing.T) {
	ms := machines(2)
	ms[0].Capacity = &v1.Resources{CpuMillis: 1000, MemoryBytes: 1 << 30}
	ms[1].Capacity = &v1.Resources{CpuMillis: 1000, MemoryBytes: 1 << 30}
	ms[0].Used = &v1.Resources{CpuMillis: 900}
	spec := &v1.ServiceSpec{Replicas: 3, Resources: &v1.Resources{CpuMillis: 400}}
	p := Schedule(spec, ms, nil)
	require.Equal(t, map[string]int{"m2": 2}, placed(p))
	require.Equal(t, 1, p.Unplaceable)
}

func TestDownMachineReschedulableVsStranded(t *testing.T) {
	ms := machines(3)
	ms[0].State = v1.MachineState_MACHINE_STATE_DOWN
	ex := []*v1.Replica{{Id: "r1", MachineId: "m1"}, {Id: "r2", MachineId: "m2"}}

	p := Schedule(&v1.ServiceSpec{Replicas: 2, Reschedulable: true}, ms, ex)
	require.Len(t, p.Remove, 1)
	require.Equal(t, "r1", p.Remove[0].Id)
	require.Equal(t, map[string]int{"m3": 1}, placed(p))

	p = Schedule(&v1.ServiceSpec{Replicas: 2}, ms, ex)
	require.Len(t, p.Stranded, 1)
	require.Empty(t, p.Place)
	require.Empty(t, p.Remove)
}

func TestDeterministic(t *testing.T) {
	a := Schedule(&v1.ServiceSpec{Replicas: 4}, machines(3), nil)
	for i := 0; i < 20; i++ {
		require.Equal(t, a.Place, Schedule(&v1.ServiceSpec{Replicas: 4}, machines(3), nil).Place)
	}
}
