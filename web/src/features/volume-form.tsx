import { Field, Input, Select } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
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
      <label className="flex items-center gap-2 text-sm">
        <Switch checked={value.readOnly} onCheckedChange={(c) => set('readOnly', c)} aria-label="Read-only mount" />
        Mount read-only
      </label>
    </div>
  )
}
