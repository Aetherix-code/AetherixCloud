/** Shared helpers for service volume forms (Settings + New Service). */

export type VolumeDraft = {
  source: string
  target: string
  persistence: 'local' | 'backup-restore'
  /** Soft size limit in MiB; 0 = unlimited / unset. */
  sizeMiB: number
  readOnly: boolean
  /** 5-field cron UTC; empty uses default daily 03:00. */
  backupCron: string
  /** Snapshots to retain; 0 = default (7). */
  backupRetain: number
  /** Settings → S3 Targets id; empty = default/first target. */
  s3TargetId: string
}

export const emptyVolume = (partial?: Partial<VolumeDraft>): VolumeDraft => ({
  source: '',
  target: '',
  persistence: 'local',
  sizeMiB: 0,
  readOnly: false,
  backupCron: '0 3 * * *',
  backupRetain: 7,
  s3TargetId: '',
  ...partial,
})

export function volumeFromSpec(v: {
  source: string
  target: string
  persistence?: string
  sizeBytes?: bigint
  readOnly?: boolean
  backupCron?: string
  backupRetain?: number
  s3TargetId?: string
}): VolumeDraft {
  return {
    source: v.source,
    target: v.target,
    persistence: v.persistence === 'backup-restore' ? 'backup-restore' : 'local',
    sizeMiB: v.sizeBytes ? Math.round(Number(v.sizeBytes) / (1024 * 1024)) : 0,
    readOnly: !!v.readOnly,
    backupCron: v.backupCron || '0 3 * * *',
    backupRetain: v.backupRetain && v.backupRetain > 0 ? v.backupRetain : 7,
    s3TargetId: v.s3TargetId || '',
  }
}

export function volumeToSpec(v: VolumeDraft) {
  const backup = v.persistence === 'backup-restore'
  return {
    source: v.source.trim(),
    target: v.target.trim(),
    persistence: backup ? 'backup-restore' : 'local',
    sizeBytes: v.sizeMiB > 0 ? BigInt(v.sizeMiB) * 1024n * 1024n : 0n,
    readOnly: v.readOnly,
    backupCron: backup ? (v.backupCron.trim() || '0 3 * * *') : '',
    backupRetain: backup ? (v.backupRetain > 0 ? v.backupRetain : 7) : 0,
    s3TargetId: backup ? v.s3TargetId : '',
  }
}

export function volumeValid(v: VolumeDraft): boolean {
  const src = v.source.trim()
  const tgt = v.target.trim()
  if (!src || !tgt) return false
  if (!tgt.startsWith('/')) return false
  if (!src.startsWith('/') && !/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(src)) return false
  if (v.sizeMiB < 0) return false
  if (v.persistence === 'backup-restore' && v.backupRetain < 0) return false
  return true
}

export const PERSISTENCE_HELP = {
  local: 'Data stays on the machine that first ran the replica. If that machine dies, the replica is stranded until the node returns — it will not reschedule.',
  'backup-restore': 'Snapshots go to an S3 target on the schedule below. On reschedule after a node failure, the latest snapshot is restored onto the new machine. Configure destinations under Settings → S3 Targets.',
} as const
