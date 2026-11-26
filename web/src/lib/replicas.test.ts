import { describe, expect, it } from 'vitest'
import { ReplicaState } from '@/gen/aetherix/v1/types_pb'
import { currentReplicas, isCurrentReplica } from './replicas'

describe('currentReplicas', () => {
  it('hides retired stopped rows but keeps in-flight and live ones', () => {
    const reps = [
      { id: 'run', desiredRunning: true, state: ReplicaState.RUNNING },
      { id: 'stopg', desiredRunning: false, state: ReplicaState.STOPPING },
      { id: 'fail', desiredRunning: false, state: ReplicaState.FAILED },
      { id: 'old', desiredRunning: false, state: ReplicaState.STOPPED },
      { id: 'ds', desiredRunning: true, state: ReplicaState.STOPPED },
    ]
    expect(reps.filter(isCurrentReplica).map((r) => r.id)).toEqual(['run', 'stopg', 'fail', 'ds'])
  })

  it('after stop then start only keeps the new running replicas', () => {
    expect(
      currentReplicas([
        { id: 'r1-old', desiredRunning: false, state: ReplicaState.STOPPED },
        { id: 'r2-old', desiredRunning: false, state: ReplicaState.STOPPED },
        { id: 'r1-new', desiredRunning: true, state: ReplicaState.RUNNING },
        { id: 'r2-new', desiredRunning: true, state: ReplicaState.RUNNING },
      ]).map((r) => r.id),
    ).toEqual(['r1-new', 'r2-new'])
  })
})
