import { useQuery } from '@tanstack/react-query'
import { Field, Input, Select } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { fetchS3Targets } from '@/lib/extras'
import { isMock } from '@/lib/api'
import { PERSISTENCE_HELP, type VolumeDraft } from '@/lib/volumes'
import { cn } from '@/lib/utils'

/** Inline editor fields for a single volume draft (add/edit). */
export function VolumeFields({ value, onChange, nameHint, className }: {
  value: VolumeDraft
  onChange: (v: VolumeDraft) => void
  nameHint?: string
  className?: string
}) {
  const set = <K extends keyof VolumeDraft>(k: K, v: VolumeDraft[K]) => onChange({ ...value, [k]: v })
  const targets = useQuery({
    queryKey: ['s3-targets'],
    queryFn: fetchS3Targets,
    retry: false,
    enabled: !isMock && value.persistence === 'backup-restore',
  })
  const list = targets.data?.targets ?? []

  return (
    <div className={cn('space-y-3', className)}>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Volume name" hint={nameHint ?? 'Docker named volume (or absolute host path for a bind mount).'}>
          <Input value={value.source} onChange={(e) => set('source', e.target.value)} placeholder="app-data" pattern="[a-zA-Z0-9][a-zA-Z0-9_.-]*|/.*" required />
        </Field>
        <Field label="Container path" hint="Absolute path inside the container.">
          <Input value={value.target} onChange={(e) => set('target', e.target.value)} placeholder="/var/lib/data" pattern="/.*" required />
        </Field>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Size limit (MiB)" hint="0 = no limit. Applied when the volume is first created (Docker local driver size opt; needs host quotas to enforce).">
          <Input type="number" min={0} value={value.sizeMiB} onChange={(e) => set('sizeMiB', Math.max(0, Number(e.target.value) || 0))} />
        </Field>
        <Field label="Backup policy">
          <Select value={value.persistence} onChange={(e) => set('persistence', e.target.value as VolumeDraft['persistence'])}>
            <option value="local">Local only (pinned to one machine)</option>
            <option value="backup-restore">Backup & restore (reschedulable)</option>
          </Select>
        </Field>
      </div>
      <p className="rounded-md border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">{PERSISTENCE_HELP[value.persistence]}</p>
      {value.persistence === 'backup-restore' && (
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="S3 target" hint="Configured under Settings → S3 Targets. Empty = default/first target.">
            <Select value={value.s3TargetId} onChange={(e) => set('s3TargetId', e.target.value)}>
              <option value="">Default target</option>
              {list.map((t) => (
                <option key={t.id} value={t.id}>{t.name} ({t.bucket})</option>
              ))}
            </Select>
          </Field>
          <Field label="Schedule (UTC cron)" hint="5-field cron. Default daily at 03:00.">
            <Input value={value.backupCron} onChange={(e) => set('backupCron', e.target.value)} placeholder="0 3 * * *" />
          </Field>
          <Field label="Retain snapshots" hint="Older objects are pruned after each successful backup.">
            <Input type="number" min={1} value={value.backupRetain} onChange={(e) => set('backupRetain', Math.max(1, Number(e.target.value) || 7))} />
          </Field>
        </div>
      )}
      <label className="flex items-center gap-2 text-sm">
        <Switch checked={value.readOnly} onCheckedChange={(c) => set('readOnly', c)} aria-label="Read-only mount" />
        Mount read-only
      </label>
    </div>
  )
}
