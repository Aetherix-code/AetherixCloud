import { describe, expect, it } from 'vitest'
import { MachineRole, MachineState } from '@/gen/aetherix/v1/types_pb'
import { isLastRoleHolder, suggestedJoinRoles } from './roles'

const m = (id: string, state: MachineState, roles: MachineRole[]) => ({ id, state, roles })

describe('isLastRoleHolder', () => {
  it('blocks when this machine is the only reachable holder', () => {
    const machines = [
      m('a', MachineState.UP, [MachineRole.WORKER, MachineRole.INGRESS]),
      m('b', MachineState.UP, [MachineRole.WORKER]),
    ]
    expect(isLastRoleHolder(machines, 'a', MachineRole.INGRESS)).toBe(true)
    expect(isLastRoleHolder(machines, 'a', MachineRole.WORKER)).toBe(false)
    expect(isLastRoleHolder(machines, 'b', MachineRole.WORKER)).toBe(false)
  })

  it('ignores DOWN peers for coverage', () => {
    const machines = [
      m('a', MachineState.UP, [MachineRole.BUILDER]),
      m('b', MachineState.DOWN, [MachineRole.BUILDER]),
    ]
    expect(isLastRoleHolder(machines, 'a', MachineRole.BUILDER)).toBe(true)
  })

  it('counts SUSPECT peers as coverage', () => {
    const machines = [
      m('a', MachineState.UP, [MachineRole.INGRESS]),
      m('b', MachineState.SUSPECT, [MachineRole.INGRESS]),
    ]
    expect(isLastRoleHolder(machines, 'a', MachineRole.INGRESS)).toBe(false)
  })
})

describe('suggestedJoinRoles', () => {
  it('defaults to worker when the cluster already has ingress and builder', () => {
    expect(
      suggestedJoinRoles([
        m('a', MachineState.UP, [MachineRole.WORKER, MachineRole.INGRESS, MachineRole.BUILDER]),
      ]),
    ).toEqual([MachineRole.WORKER])
  })

  it('pre-selects missing essential roles', () => {
    expect(suggestedJoinRoles([m('a', MachineState.UP, [MachineRole.WORKER])])).toEqual([
      MachineRole.WORKER,
      MachineRole.INGRESS,
      MachineRole.BUILDER,
    ])
    expect(suggestedJoinRoles([])).toEqual([MachineRole.WORKER, MachineRole.INGRESS, MachineRole.BUILDER])
    expect(suggestedJoinRoles(undefined)).toEqual([MachineRole.WORKER, MachineRole.INGRESS, MachineRole.BUILDER])
  })

  it('treats DOWN-only holders as missing', () => {
    expect(suggestedJoinRoles([m('a', MachineState.DOWN, [MachineRole.INGRESS, MachineRole.BUILDER])])).toEqual([
      MachineRole.WORKER,
      MachineRole.INGRESS,
      MachineRole.BUILDER,
    ])
  })
})
