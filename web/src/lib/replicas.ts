import { ReplicaState, type Replica } from '@/gen/aetherix/v1/types_pb'

/** Match API GetService/ListReplicas: hide retired STOPPED rows left after stop/restart. */
export function isCurrentReplica(r: Pick<Replica, 'desiredRunning' | 'state'>): boolean {
  return r.desiredRunning || r.state !== ReplicaState.STOPPED
}

export function currentReplicas<T extends Pick<Replica, 'desiredRunning' | 'state'>>(reps: T[]): T[] {
  return reps.filter(isCurrentReplica)
}
