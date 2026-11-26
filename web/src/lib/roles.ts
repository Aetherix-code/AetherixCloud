import { MachineRole, MachineState, type Machine } from '@/gen/aetherix/v1/types_pb'

/** Roles the cluster must keep on at least one reachable machine. */
export const essentialRoles = [MachineRole.WORKER, MachineRole.INGRESS, MachineRole.BUILDER] as const

export type EssentialRole = (typeof essentialRoles)[number]

type RoleMachine = Pick<Machine, 'id' | 'state' | 'roles'>

/** DOWN machines are excluded from role coverage; UP/SUSPECT/UNSPECIFIED count. */
export function machineCountsForRoles(m: Pick<Machine, 'state'>): boolean {
  return m.state !== MachineState.DOWN
}

export function machineHasRole(m: Pick<Machine, 'roles'>, role: MachineRole): boolean {
  return m.roles.includes(role)
}

/** True when this machine is the only reachable holder of the role. */
export function isLastRoleHolder(machines: RoleMachine[], machineId: string, role: MachineRole): boolean {
  const holders = machines.filter((m) => machineCountsForRoles(m) && machineHasRole(m, role))
  return holders.length === 1 && holders[0]?.id === machineId
}

/**
 * Sensible join defaults: always worker, plus any essential role the cluster
 * currently lacks among reachable machines.
 */
export function suggestedJoinRoles(machines: RoleMachine[] | undefined): MachineRole[] {
  const roles: MachineRole[] = [MachineRole.WORKER]
  const list = machines ?? []
  for (const role of essentialRoles) {
    if (role === MachineRole.WORKER) continue
    const covered = list.some((m) => machineCountsForRoles(m) && machineHasRole(m, role))
    if (!covered) roles.push(role)
  }
  return roles
}
