import { Badge, type Tone } from '@/components/ui/badge'
import { DeploymentStatus, MachineRole, MachineState, ReplicaState, Role } from '@/gen/aetherix/v1/types_pb'
import { cn } from './utils'

export const machineTone: Record<MachineState, Tone> = {
  [MachineState.UNSPECIFIED]: 'neutral',
  [MachineState.UP]: 'success',
  [MachineState.SUSPECT]: 'warning',
  [MachineState.DOWN]: 'danger',
}
export const machineLabel: Record<MachineState, string> = { 0: 'unknown', 1: 'up', 2: 'suspect', 3: 'down' }

export const deployTone: Record<DeploymentStatus, Tone> = {
  [DeploymentStatus.UNSPECIFIED]: 'neutral',
  [DeploymentStatus.QUEUED]: 'neutral',
  [DeploymentStatus.BUILDING]: 'info',
  [DeploymentStatus.DEPLOYING]: 'info',
  [DeploymentStatus.ACTIVE]: 'success',
  [DeploymentStatus.FAILED]: 'danger',
  [DeploymentStatus.ROLLED_BACK]: 'warning',
  [DeploymentStatus.SUPERSEDED]: 'neutral',
}
export const deployLabel: Record<DeploymentStatus, string> = { 0: 'unknown', 1: 'queued', 2: 'building', 3: 'deploying', 4: 'active', 5: 'failed', 6: 'rolled back', 7: 'superseded' }

export const replicaTone: Record<ReplicaState, Tone> = {
  [ReplicaState.UNSPECIFIED]: 'neutral',
  [ReplicaState.PENDING]: 'neutral',
  [ReplicaState.STARTING]: 'info',
  [ReplicaState.RUNNING]: 'success',
  [ReplicaState.STOPPING]: 'warning',
  [ReplicaState.STOPPED]: 'neutral',
  [ReplicaState.FAILED]: 'danger',
  [ReplicaState.STRANDED]: 'warning',
}
export const replicaLabel: Record<ReplicaState, string> = { 0: 'unknown', 1: 'pending', 2: 'starting', 3: 'running', 4: 'stopping', 5: 'stopped', 6: 'failed', 7: 'stranded' }

export const roleLabel: Record<MachineRole, string> = { 0: '?', 1: 'worker', 2: 'ingress', 3: 'builder' }

/** Short explanation shown under each role switch (add-machine / machine page). */
export const roleDescription: Record<MachineRole, string> = {
  0: '',
  1: 'Runs service replicas on this machine.',
  2: 'Runs Caddy and receives public HTTPS traffic for the cluster.',
  3: 'May claim build leases and run Docker image builds.',
}
export const userRoleLabel: Record<Role, string> = { 0: 'unspecified', 1: 'viewer', 2: 'developer', 3: 'admin', 4: 'owner' }

export const toneDot: Record<Tone, string> = {
  neutral: 'bg-muted-foreground',
  success: 'bg-success',
  warning: 'bg-warning',
  danger: 'bg-destructive',
  info: 'bg-primary',
}

export function Dot({ tone, pulse }: { tone: Tone; pulse?: boolean }) {
  return <span className={cn('inline-block size-2 rounded-full', toneDot[tone], pulse && 'ax-pulse')} />
}

export const MachineBadge = ({ state }: { state: MachineState }) => (
  <Badge tone={machineTone[state]}>
    <Dot tone={machineTone[state]} />
    {machineLabel[state]}
  </Badge>
)
export const DeployBadge = ({ status }: { status: DeploymentStatus }) => (
  <Badge tone={deployTone[status]}>
    <Dot tone={deployTone[status]} pulse={status === DeploymentStatus.BUILDING || status === DeploymentStatus.DEPLOYING} />
    {deployLabel[status]}
  </Badge>
)
/** Prefer message-aware label when health is failing (still STARTING in proto). */
export function replicaDisplay(state: ReplicaState, message?: string): { tone: Tone; label: string } {
  const msg = (message || '').toLowerCase()
  if (msg.includes('health check fail')) {
    return { tone: 'danger', label: msg.includes('giving up') ? 'unhealthy (failed)' : 'unhealthy' }
  }
  if (msg.includes('waiting for health')) {
    return { tone: 'warning', label: 'awaiting health' }
  }
  return { tone: replicaTone[state], label: replicaLabel[state] }
}

export const ReplicaBadge = ({ state, message }: { state: ReplicaState; message?: string }) => {
  const { tone, label } = replicaDisplay(state, message)
  return (
    <Badge tone={tone} title={message || undefined}>
      <Dot tone={tone} pulse={tone === 'danger' || label === 'awaiting health'} />
      {label}
    </Badge>
  )
}
