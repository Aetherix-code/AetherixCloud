package api

import (
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/stretchr/testify/require"
)

func TestEnsureEssentialRoleCoverage(t *testing.T) {
	workerIngressBuilder := []v1.MachineRole{
		v1.MachineRole_MACHINE_ROLE_WORKER,
		v1.MachineRole_MACHINE_ROLE_INGRESS,
		v1.MachineRole_MACHINE_ROLE_BUILDER,
	}
	a := &v1.Machine{Id: "a", State: v1.MachineState_MACHINE_STATE_UP, Roles: append([]v1.MachineRole{}, workerIngressBuilder...)}
	b := &v1.Machine{Id: "b", State: v1.MachineState_MACHINE_STATE_UP, Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}}

	// Sole holder cannot drop any essential role.
	err := ensureEssentialRoleCoverage([]*v1.Machine{a}, a, []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER})
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.ErrorContains(t, err, "ingress")

	err = ensureEssentialRoleCoverage([]*v1.Machine{a}, a, nil)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	// Adding roles is always fine.
	require.NoError(t, ensureEssentialRoleCoverage([]*v1.Machine{b}, b, workerIngressBuilder))

	// Another reachable holder allows removing the role.
	a2 := &v1.Machine{Id: "a", State: v1.MachineState_MACHINE_STATE_UP, Roles: append([]v1.MachineRole{}, workerIngressBuilder...)}
	b2 := &v1.Machine{Id: "b", State: v1.MachineState_MACHINE_STATE_UP, Roles: append([]v1.MachineRole{}, workerIngressBuilder...)}
	require.NoError(t, ensureEssentialRoleCoverage([]*v1.Machine{a2, b2}, a2, []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}))

	// DOWN peers do not count as coverage.
	down := &v1.Machine{Id: "d", State: v1.MachineState_MACHINE_STATE_DOWN, Roles: append([]v1.MachineRole{}, workerIngressBuilder...)}
	err = ensureEssentialRoleCoverage([]*v1.Machine{a, down}, a, []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER})
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.ErrorContains(t, err, "ingress")

	// SUSPECT peers still count.
	suspect := &v1.Machine{Id: "s", State: v1.MachineState_MACHINE_STATE_SUSPECT, Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_INGRESS, v1.MachineRole_MACHINE_ROLE_BUILDER}}
	require.NoError(t, ensureEssentialRoleCoverage([]*v1.Machine{a, suspect}, a, []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}))

	// Already-missing roles are not forced back; only removals are gated.
	noBuilder := &v1.Machine{Id: "n", State: v1.MachineState_MACHINE_STATE_UP, Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER, v1.MachineRole_MACHINE_ROLE_INGRESS}}
	other := &v1.Machine{Id: "o", State: v1.MachineState_MACHINE_STATE_UP, Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER, v1.MachineRole_MACHINE_ROLE_INGRESS}}
	require.NoError(t, ensureEssentialRoleCoverage([]*v1.Machine{noBuilder, other}, noBuilder, []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}))
}
