import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { ArrowLeft, Check, Copy, Loader2, Terminal } from 'lucide-react'
import { Page, PageHeader } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { toast } from '@/components/ui/toast'
import { Field, Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { MachineRole } from '@/gen/aetherix/v1/types_pb'
import { api } from '@/lib/api'
import { keys, useAction, useMachines } from '@/lib/queries'
import { suggestedJoinRoles } from '@/lib/roles'
import { roleDescription, roleLabel } from '@/lib/status'
import { copy } from '@/lib/utils'

const roleOptions = [MachineRole.WORKER, MachineRole.INGRESS, MachineRole.BUILDER] as const

/** Add-machine wizard: 1) name + roles, 2) run the join command, 3) wait for the node to appear. */
export function AddMachinePage() {
  const [name, setName] = useState('')
  const [roles, setRoles] = useState<MachineRole[] | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const machines = useMachines()
  const baseline = useRef<Set<string>>(new Set())
  const selected = roles ?? suggestedJoinRoles(machines.data)

  const create = useAction(() => api.machines.createJoinToken({ ttlSeconds: 3600, roles: selected, machineName: name }), {
    invalidate: [keys.machines],
    onSuccess: () => {
      baseline.current = new Set((machines.data ?? []).map((m) => m.id))
    },
  })
  const t = create.data
  useEffect(() => {
    if (!t) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [t])

  const joined = t ? machines.data?.find((m) => !baseline.current.has(m.id) && (!name || m.name === name)) : undefined
  const remaining = t ? Math.max(0, Number(t.expiresUnix) - Math.floor(now / 1000)) : 0
  const step = joined ? 3 : t ? 2 : 1

  return (
    <Page>
      <Link to="/cluster" className="mb-3 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"><ArrowLeft className="size-4" />Cluster</Link>
      <PageHeader title="Add a machine" hideTitle description="Any Linux server with Docker access. It joins over WireGuard and syncs the cluster state." />
      <ol className="mb-6 flex items-center gap-2 text-xs" aria-label="Steps">
        {['Configure', 'Run command', 'Connected'].map((s, i) => (
          <li key={s} className="flex items-center gap-2">
            <span className={`grid size-5 place-items-center rounded-full border text-[11px] ${step > i + 1 ? 'border-success bg-success text-white' : step === i + 1 ? 'border-primary text-primary' : 'text-muted-foreground'}`}>{step > i + 1 ? <Check className="size-3" /> : i + 1}</span>
            <span className={step === i + 1 ? 'font-medium' : 'text-muted-foreground'}>{s}</span>
            {i < 2 && <span className="mx-1 h-px w-8 bg-border" />}
          </li>
        ))}
      </ol>
      {step === 1 && (
        <Card><CardContent>
          <form className="space-y-4" onSubmit={(e) => { e.preventDefault(); create.mutate(undefined) }}>
            <Field label="Machine name" hint="Optional. Defaults to the hostname."><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="node-4" pattern="[a-z0-9][a-z0-9-]*" /></Field>
            <div className="space-y-2">
              <span className="text-xs font-medium text-muted-foreground">Roles</span>
              <div className="divide-y rounded-md border">
                {roleOptions.map((r) => {
                  const on = selected.includes(r)
                  return (
                    <div key={r} className="flex items-start justify-between gap-4 px-3 py-2.5">
                      <div className="min-w-0">
                        <div className="text-sm font-medium capitalize">{roleLabel[r]}</div>
                        <p className="text-xs text-muted-foreground">{roleDescription[r]}</p>
                      </div>
                      <Switch
                        checked={on}
                        onCheckedChange={(v) => setRoles(v ? [...selected, r] : selected.filter((x) => x !== r))}
                        aria-label={roleLabel[r]}
                      />
                    </div>
                  )
                })}
              </div>
            </div>
            <p className="text-xs text-muted-foreground">Installing over SSH from this dashboard isn’t available yet; use the one-line command on the new server.</p>
            <Button type="submit" disabled={create.isPending || !selected.length}>Generate join command</Button>
          </form>
        </CardContent></Card>
      )}
      {step === 2 && t && (
        <Card><CardContent className="space-y-4">
          <div>
            <div className="mb-1.5 flex items-center gap-2 text-sm font-medium"><Terminal className="size-4" />Run on the new server (as root)</div>
            <div className="relative rounded-md border bg-muted p-3 pr-12 font-mono text-xs break-all" data-testid="join-command">{t.installCommand}</div>
            <Button size="icon" variant="ghost" className="-mt-11 mr-2 float-right" aria-label="Copy command" onClick={() => { void copy(t.installCommand); toast.success('Copied') }}><Copy /></Button>
          </div>
          <p className="text-xs text-muted-foreground">{remaining > 0 ? `Token expires in ${Math.floor(remaining / 60)}m ${remaining % 60}s.` : 'This token has expired. Generate a new one.'} Needs UDP 51820 open.</p>
          <div className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />Waiting for the machine to join…</div>
        </CardContent></Card>
      )}
      {step === 3 && joined && (
        <Card><CardContent className="flex flex-col items-center gap-3 py-8 text-center">
          <div className="grid size-10 place-items-center rounded-full bg-success text-white"><Check /></div>
          <p className="font-medium">{joined.name} joined the cluster</p>
          <Button asChild><Link to="/cluster/machines/$machineId" params={{ machineId: joined.id }}>Open machine</Link></Button>
        </CardContent></Card>
      )}
    </Page>
  )
}
