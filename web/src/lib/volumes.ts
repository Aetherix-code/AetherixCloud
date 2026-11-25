/** Shared helpers for service volume forms (Settings + New Service). */

export type VolumeDraft = {
  source: string
  target: string
  persistence: 'local' | 'backup-restore'
  /** Soft size limit in MiB; 0 = unlimited / unset. */
  sizeMiB: number
  readOnly: boolean
}

export const emptyVolume = (partial?: Partial<VolumeDraft>): VolumeDraft => ({
  source: '',
  target: '',
  persistence: 'local',
  sizeMiB: 0,
  readOnly: false,
  ...partial,
})

export function volumeFromSpec(v: { source: string; target: string; persistence?: string; sizeBytes?: bigint; readOnly?: boolean }): VolumeDraft {
  return {
    source: v.source,
    target: v.target,
    persistence: v.persistence === 'backup-restore' ? 'backup-restore' : 'local',
    sizeMiB: v.sizeBytes ? Math.round(Number(v.sizeBytes) / (1024 * 1024)) : 0,
    readOnly: !!v.readOnly,
  }
}

export function volumeToSpec(v: VolumeDraft) {
  return {
    source: v.source.trim(),
    target: v.target.trim(),
    persistence: v.persistence === 'backup-restore' ? 'backup-restore' : 'local',
    sizeBytes: v.sizeMiB > 0 ? BigInt(v.sizeMiB) * 1024n * 1024n : 0n,
    readOnly: v.readOnly,
  }
}

export function volumeValid(v: VolumeDraft): boolean {
  const src = v.source.trim()
  const tgt = v.target.trim()
  if (!src || !tgt) return false
  if (!tgt.startsWith('/')) return false
  if (!src.startsWith('/') && !/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(src)) return false
  if (v.sizeMiB < 0) return false
  return true
}

export const PERSISTENCE_HELP = {
  local: 'Data stays on the machine that first ran the replica. If that machine dies, the replica is stranded until the node returns — it will not reschedule.',
  'backup-restore': 'Cluster backups (Settings → Backups) snapshot this volume to S3. On reschedule after a node failure, the latest snapshot is restored onto the new machine.',
} as const
