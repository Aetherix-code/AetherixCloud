package api

import (
	"fmt"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
)

// essentialMachineRoles must stay covered by at least one reachable machine.
var essentialMachineRoles = []v1.MachineRole{
	v1.MachineRole_MACHINE_ROLE_WORKER,
	v1.MachineRole_MACHINE_ROLE_INGRESS,
	v1.MachineRole_MACHINE_ROLE_BUILDER,
}

func machineRoleName(r v1.MachineRole) string {
	switch r {
	case v1.MachineRole_MACHINE_ROLE_WORKER:
		return "worker"
	case v1.MachineRole_MACHINE_ROLE_INGRESS:
		return "ingress"
	case v1.MachineRole_MACHINE_ROLE_BUILDER:
		return "builder"
	default:
		return r.String()
	}
}

func machineHasRole(roles []v1.MachineRole, role v1.MachineRole) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// machineCountsForRoles reports whether m is considered for cluster role
// coverage. DOWN machines are excluded; UP/SUSPECT/UNSPECIFIED count.
func machineCountsForRoles(m *v1.Machine) bool {
	return m != nil && m.State != v1.MachineState_MACHINE_STATE_DOWN
}

// ensureEssentialRoleCoverage rejects removing the last reachable holder of an
// essential role (worker, ingress, builder). Turning a role on is always fine;
// roles the cluster already lacks are not required to stay absent.
func ensureEssentialRoleCoverage(machines []*v1.Machine, updated *v1.Machine, newRoles []v1.MachineRole) error {
	if updated == nil {
		return nil
	}
	for _, role := range essentialMachineRoles {
		if !machineHasRole(updated.Roles, role) || machineHasRole(newRoles, role) {
			continue
		}
		if otherReachableRoleHolder(machines, updated.Id, role) {
			continue
		}
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("cannot remove the last %s role from the cluster", machineRoleName(role)))
	}
	return nil
}

func otherReachableRoleHolder(machines []*v1.Machine, exceptID string, role v1.MachineRole) bool {
	for _, m := range machines {
		if m == nil || m.Id == exceptID || !machineCountsForRoles(m) {
			continue
		}
		if machineHasRole(m.Roles, role) {
			return true
		}
	}
	return false
}
