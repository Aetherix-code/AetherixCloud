import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Eye, EyeOff, Link2, Plus, Trash2 } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { CheckboxField } from '@/components/ui/checkbox'
import { Input, Select, Textarea } from '@/components/ui/input'
import { ListSkeleton, QueryError } from '@/components/ui/states'
import type { Service, Variable } from '@/gen/aetherix/v1/types_pb'
import { api } from '@/lib/api'
import { keys, useAction, useVariables } from '@/lib/queries'
import { parseEnvText, parseRefs, toEnvText } from '@/lib/refs'
import { cn } from '@/lib/utils'

interface Row {
  key: string
  value: string
  secret: boolean
  redacted: boolean
  isNew: boolean
}

const toRows = (vars: Variable[]): Row[] => vars.map((v) => ({ key: v.key, value: v.value, secret: v.secret, redacted: v.redacted, isNew: false }))

export function VariablesTab({ service, environmentId, services }: { service: Service; environmentId: string; services: Service[] }) {
  const q = useVariables(environmentId, service.id)
  const all = useQuery({ queryKey: keys.variables(environmentId, '*'), queryFn: async () => (await api.projects.listVariables({ environmentId, serviceId: '*' })).variables })
  if (q.isLoading) return <ListSkeleton rows={5} />
  if (q.error) return <QueryError error={q.error} what="variables" />
  // Re-key on server data so edits reset after a successful save.
  const version = (q.data ?? []).map((v) => `${v.key}=${v.value}:${v.secret}`).join('|')
  return <Editor key={version} service={service} environmentId={environmentId} initial={toRows(q.data ?? [])} services={services} allVars={all.data ?? []} />
}

function Editor({ service, environmentId, initial, services, allVars }: { service: Service; environmentId: string; initial: Row[]; services: Service[]; allVars: Variable[] }) {
  const [rows, setRows] = useState<Row[]>(initial)
  const [raw, setRaw] = useState(false)
  const [rawText, setRawText] = useState('')
  const [reveal, setReveal] = useState<Set<string>>(new Set())
  const [redeploy, setRedeploy] = useState(true)
  const [refSvc, setRefSvc] = useState('')
  const [refKey, setRefKey] = useState('')

  const dirty = useMemo(() => JSON.stringify(rows) !== JSON.stringify(initial), [rows, initial])
  const inv = [keys.variables(environmentId, service.id), keys.variables(environmentId, '*')]
  const save = useAction(
    async () => {
      const removed = initial.filter((i) => !rows.some((r) => r.key === i.key))
      for (const r of removed) await api.projects.deleteVariable({ environmentId, serviceId: service.id, key: r.key })
      const changed = rows.filter((r) => r.key && (r.isNew || !initial.some((i) => i.key === r.key && i.value === r.value && i.secret === r.secret)))
      if (changed.length || removed.length)
        await api.projects.setVariables({
          environmentId,
          serviceId: service.id,
          redeploy,
          variables: changed.map((r) => ({ key: r.key, value: r.value, secret: r.secret, redacted: r.redacted && !r.value })),
        })
    },
    { invalidate: inv, success: redeploy ? 'Variables saved; redeploying' : 'Variables saved' },
  )

  const update = (i: number, patch: Partial<Row>) => setRows(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)))
  const refCandidates = allVars.filter((v) => services.find((s) => s.id === v.serviceId)?.name === refSvc)
  const insertRef = () => {
    if (!refSvc || !refKey) return
    const token = `\${{${refSvc}.${refKey}}}`
    if (raw) setRawText((t) => `${t}${t && !t.endsWith('\n') ? '\n' : ''}NEW_VAR=${token}`)
    else setRows([...rows, { key: 'NEW_VAR', value: token, secret: false, redacted: false, isNew: true }])
  }
  const switchMode = (toRaw: boolean) => {
    if (toRaw) setRawText(toEnvText(rows.map((r) => ({ ...r, redacted: r.redacted && !r.value }))))
    else {
      const parsed = parseEnvText(rawText)
      setRows(parsed.map((p) => {
        const ex = rows.find((r) => r.key === p.key)
        return { key: p.key, value: p.value, secret: ex?.secret ?? false, redacted: !!ex?.redacted && !p.value, isNew: !ex }
      }))
    }
    setRaw(toRaw)
  }
  const doSave = () => {
    if (raw) switchMode(false)
    setTimeout(() => save.mutate(undefined), 0)
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" variant={raw ? 'outline' : 'secondary'} onClick={() => raw && switchMode(false)}>Table</Button>
        <Button size="sm" variant={raw ? 'secondary' : 'outline'} onClick={() => !raw && switchMode(true)}>Raw editor</Button>
        <div className="ml-auto flex items-center gap-1.5">
          <Select className="h-8 w-auto text-xs" value={refSvc} onChange={(e) => { setRefSvc(e.target.value); setRefKey('') }} aria-label="Reference service">
            <option value="">Reference…</option>
            {services.filter((s) => s.id !== service.id).map((s) => <option key={s.id} value={s.name}>{s.name}</option>)}
          </Select>
          {refSvc && (
            <Select className="h-8 w-auto text-xs" value={refKey} onChange={(e) => setRefKey(e.target.value)} aria-label="Reference key">
              <option value="">key…</option>
              {refCandidates.map((v) => <option key={v.id} value={v.key}>{v.key}</option>)}
            </Select>
          )}
          <Button size="sm" variant="outline" onClick={insertRef} disabled={!refKey} title="Insert ${{service.KEY}}"><Link2 />Insert</Button>
        </div>
      </div>

      {raw ? (
        <Textarea rows={14} value={rawText} onChange={(e) => setRawText(e.target.value)} spellCheck={false} aria-label="Raw variables" placeholder={'KEY=value\nDATABASE_URL=${{postgres.DATABASE_URL}}'} />
      ) : (
        <div className="divide-y rounded-lg border">
          {rows.length === 0 && <p className="p-4 text-sm text-muted-foreground">No variables yet.</p>}
          {rows.map((r, i) => {
            const isRevealed = reveal.has(r.key)
            const refs = parseRefs(r.value)
            return (
              <div key={i} className="flex items-start gap-2 p-2">
                <Input className="w-2/5 font-mono text-xs" value={r.key} onChange={(e) => update(i, { key: e.target.value.toUpperCase() })} placeholder="KEY" aria-label="Key" />
                <div className="min-w-0 flex-1 space-y-1">
                  <Input
                    className={cn('font-mono text-xs')}
                    type={r.secret && !isRevealed ? 'password' : 'text'}
                    value={r.value}
                    onChange={(e) => update(i, { value: e.target.value, redacted: false })}
                    placeholder={r.redacted ? '•••••• (hidden, type to replace)' : 'value'}
                    aria-label="Value"
                  />
                  {refs.length > 0 && <div className="flex flex-wrap gap-1">{refs.map((x, j) => <Badge key={j} tone="info">→ {x.service}.{x.key}</Badge>)}</div>}
                </div>
                <Button variant="ghost" size="icon" className="size-8" title={r.secret ? 'Secret (never shown after saving)' : 'Mark as secret'} aria-pressed={r.secret} onClick={() => update(i, { secret: !r.secret })}>
                  {r.secret ? <EyeOff /> : <Eye />}
                </Button>
                {r.secret && r.value && (
                  <Button variant="ghost" size="sm" className="h-8 px-2 text-xs" onClick={() => setReveal(new Set(isRevealed ? [...reveal].filter((k) => k !== r.key) : [...reveal, r.key]))}>{isRevealed ? 'hide' : 'show'}</Button>
                )}
                <Button variant="ghost" size="icon" className="size-8" aria-label={`Delete ${r.key}`} onClick={() => setRows(rows.filter((_, j) => j !== i))}><Trash2 /></Button>
              </div>
            )
          })}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-3">
        {!raw && <Button size="sm" variant="outline" onClick={() => setRows([...rows, { key: '', value: '', secret: false, redacted: false, isNew: true }])}><Plus />Add variable</Button>}
        <CheckboxField className="ml-auto text-xs text-muted-foreground" checked={redeploy} onCheckedChange={setRedeploy}>
          Redeploy after saving
        </CheckboxField>
        <Button size="sm" onClick={doSave} disabled={(!dirty && !raw) || save.isPending}>{save.isPending ? 'Saving…' : 'Save changes'}</Button>
      </div>
      <p className="text-xs text-muted-foreground">Variables are available at build time and at runtime. Use <code className="rounded bg-muted px-1">{'${{service.KEY}}'}</code> to reference another service’s variable. References also draw the edges on the project canvas. Secret values are never sent back to the browser.</p>
    </div>
  )
}
