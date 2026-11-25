import { useState } from 'react'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ArrowLeft, Cpu, Plus, Server, ShieldBan, Trash2, Waves } from 'lucide-react'
import { Page, PageHeader } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogHeader } from '@/components/ui/dialog'
import { CheckboxField } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Empty, ListSkeleton, QueryError } from '@/components/ui/states'
import { MachineRole, MachineState, ReplicaState, type Machine } from '@/gen/aetherix/v1/types_pb'
import { api } from '@/lib/api'
import { keys, useAction, useMachines, useStatus } from '@/lib/queries'
import { MachineBadge, ReplicaBadge, roleLabel } from '@/lib/status'
import { cn, formatBytes, formatCpu, pct, timeAgo } from '@/lib/utils'

const machineRoute = getRouteApi('/app/cluster/machines/$machineId')

function Bar({ label, value, text }: { label: string; value: number; text: string }) {
  return (
    <div>
      <div className="mb-1 flex justify-between text-[11px] text-muted-foreground"><span>{label}</span><span>{text}</span></div>
      <div className="h-1.5 overflow-hidden rounded bg-muted" role="progressbar" aria-valuenow={value} aria-valuemin={0} aria-valuemax={100} aria-label={label}>
        <div className={cn('h-full rounded', value > 85 ? 'bg-destructive' : value > 65 ? 'bg-warning' : 'bg-primary')} style={{ width: `${value}%` }} />
      </div>
    </div>
  )
}

function MachineCard({ m }: { m: Machine }) {
  const { data: reps } = useQuery({ queryKey: ['replicas', 'machine', m.id], queryFn: async () => (await api.services.listReplicas({ machineId: m.id })).replicas })
  return (
    <Link to="/cluster/machines/$machineId" params={{ machineId: m.id }}>
      <Card className={cn('ax-row h-full p-4 transition-colors hover:border-primary/60', m.state === MachineState.DOWN && 'border-destructive/40')}>
        <div className="flex items-center gap-2">
          <Server className="size-4 text-muted-foreground" />
          <span className="font-medium">{m.name}</span>
          <span className="ml-auto flex items-center gap-1.5">
            {m.cordoned && <Badge tone="warning">cordoned</Badge>}
            {m.draining && <Badge tone="info">draining</Badge>}
            <MachineBadge state={m.state} />
          </span>
        </div>
        <div className="mt-1 text-xs text-muted-foreground">
          {m.meshIp} · seen {timeAgo(m.lastSeen)}
          {m.agentVersion ? <> · <span className="font-mono">v{m.agentVersion.replace(/^v/, '')}</span></> : null}
        </div>
        <div className="mt-3 space-y-2">
          <Bar label="CPU" value={pct(m.used?.cpuMillis ?? 0, m.capacity?.cpuMillis ?? 0)} text={`${formatCpu(m.used?.cpuMillis ?? 0)} / ${formatCpu(m.capacity?.cpuMillis ?? 0)}`} />
          <Bar label="Memory" value={pct(m.used?.memoryBytes ?? 0, m.capacity?.memoryBytes ?? 0)} text={`${formatBytes(m.used?.memoryBytes ?? 0)} / ${formatBytes(m.capacity?.memoryBytes ?? 0)}`} />
          <Bar label="Disk" value={pct(m.used?.diskBytes ?? 0, m.capacity?.diskBytes ?? 0)} text={`${formatBytes(m.used?.diskBytes ?? 0)} / ${formatBytes(m.capacity?.diskBytes ?? 0)}`} />
        </div>
        <div className="mt-3 flex flex-wrap items-center gap-1.5">
          {m.roles.map((r) => <Badge key={r} tone="info">{roleLabel[r]}</Badge>)}
          <span className="ml-auto text-xs text-muted-foreground">{reps?.length ?? 0} replicas</span>
        </div>
      </Card>
    </Link>
  )
}

export function ClusterPage() {
  const q = useMachines()
  const st = useStatus()
  const versions = [...new Set((q.data ?? []).map((m) => m.agentVersion?.replace(/^v/, '') || '').filter(Boolean))]
  const skew = versions.length > 1
  return (
    <Page wide>
      <PageHeader
        title="Cluster"
        description={st.data ? `${st.data.machinesUp}/${st.data.machinesTotal} machines up · ${st.data.replicasRunning}/${st.data.replicasDesired} replicas running · ${st.data.cluster?.network}` : undefined}
        actions={
          <div className="flex flex-wrap gap-2">
            <Button asChild variant="outline"><Link to="/settings/updates"><Cpu className="size-4" />Updates{skew ? <Badge tone="warning">skew</Badge> : null}</Link></Button>
            <Button asChild><Link to="/cluster/add"><Plus />Add machine</Link></Button>
          </div>
        }
      />
      {skew && (
        <Card className="mb-4 border-warning/35 bg-warning/5 px-4 py-3 text-sm">
          <div className="font-medium tracking-tight">Version skew detected</div>
          <p className="mt-0.5 text-muted-foreground">
            Machines report {versions.map((v) => `v${v}`).join(', ')}. Align them from{' '}
            <Link to="/settings/updates" className="underline-offset-2 hover:underline">Updates</Link>.
          </p>
        </Card>
      )}
      {q.isLoading ? <ListSkeleton /> : q.error ? <QueryError error={q.error} what="machines" /> : !q.data?.length ? (
        <Card>
          <Empty title="No machines" icon={<Server className="size-8" />}>
            Add the first machine to schedule replicas and serve traffic.
            <div className="mt-4"><Button size="sm" asChild><Link to="/cluster/add"><Plus />Add machine</Link></Button></div>
          </Empty>
        </Card>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3" aria-label="Machine map">{q.data.map((m) => <MachineCard key={m.id} m={m} />)}</div>
      )}
    </Page>
  )
}

export function MachinePage() {
  const { machineId } = machineRoute.useParams()
  const nav = useNavigate()
  const q = useQuery({ queryKey: ['machine', machineId], queryFn: async () => (await api.machines.getMachine({ machine: machineId })).machine, refetchInterval: 8000 })
  const reps = useQuery({ queryKey: ['replicas', 'machine', machineId], queryFn: async () => (await api.services.listReplicas({ machineId })).replicas })
  const svcs = useQuery({ queryKey: ['services', 'all'], queryFn: async () => (await api.services.listServices({})).services })
  const inv = [['machine', machineId], keys.machines, ['replicas']]
  const cordon = useAction((c: boolean) => api.machines.cordonMachine({ machine: machineId, cordoned: c }), { invalidate: inv, success: 'Updated' })
  const drain = useAction(() => api.machines.drainMachine({ machine: machineId }), {
    invalidate: inv,
    success: (r) => `Drained: ${r.replicasMoved} replicas moved${r.strandedServiceIds.length ? `, ${r.strandedServiceIds.length} stranded` : ''}`,
  })
  const setRoles = useAction((roles: MachineRole[]) => api.machines.updateMachine({ machine: machineId, roles, setRoles: true }), { invalidate: inv, success: 'Roles updated' })
  const rename = useAction((name: string) => api.machines.updateMachine({ machine: machineId, name }), { invalidate: inv, success: 'Renamed' })
  const [removing, setRemoving] = useState(false)
  const [force, setForce] = useState(false)
  const [name, setName] = useState('')
  const remove = useAction(() => api.machines.removeMachine({ machine: machineId, force }), { invalidate: [keys.machines], success: 'Machine removed', onSuccess: () => void nav({ to: '/cluster' }) })

  if (q.isLoading) return <Page><ListSkeleton /></Page>
  if (q.error || !q.data) return <Page><QueryError error={q.error} what="machine" /></Page>
  const m = q.data
  const toggleRole = (r: MachineRole) => setRoles.mutate(m.roles.includes(r) ? m.roles.filter((x) => x !== r) : [...m.roles, r])
  const svcName = (id: string) => svcs.data?.find((s) => s.id === id)?.name ?? id

  return (
    <Page>
      <Link to="/cluster" className="mb-3 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"><ArrowLeft className="size-4" />Cluster</Link>
      <PageHeader
        title={m.name}
        description={`${m.id} · ${m.meshIp}${m.agentVersion ? ` · aetherixd ${m.agentVersion}` : ''}${m.upgradeStatus ? ` · ${m.upgradeStatus}` : ''}`}
        actions={
          <>
            <MachineBadge state={m.state} />
            <Button variant="outline" size="sm" onClick={() => cordon.mutate(!m.cordoned)}><ShieldBan />{m.cordoned ? 'Uncordon' : 'Cordon'}</Button>
            <Button variant="outline" size="sm" onClick={() => drain.mutate(undefined)} disabled={drain.isPending}>Drain</Button>
            <Button variant="destructive" size="sm" onClick={() => setRemoving(true)}><Trash2 />Remove</Button>
          </>
        }
      />
      <div className="grid gap-4 md:grid-cols-2">
        <Card>
          <CardHeader><CardTitle className="flex items-center gap-2"><Cpu className="size-4" />Resources</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <Bar label="CPU" value={pct(m.used?.cpuMillis ?? 0, m.capacity?.cpuMillis ?? 0)} text={`${formatCpu(m.used?.cpuMillis ?? 0)} / ${formatCpu(m.capacity?.cpuMillis ?? 0)}`} />
            <Bar label="Memory" value={pct(m.used?.memoryBytes ?? 0, m.capacity?.memoryBytes ?? 0)} text={`${formatBytes(m.used?.memoryBytes ?? 0)} / ${formatBytes(m.capacity?.memoryBytes ?? 0)}`} />
            <Bar label="Disk" value={pct(m.used?.diskBytes ?? 0, m.capacity?.diskBytes ?? 0)} text={`${formatBytes(m.used?.diskBytes ?? 0)} / ${formatBytes(m.capacity?.diskBytes ?? 0)}`} />
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Roles</CardTitle></CardHeader>
          <CardContent className="space-y-2">
            {[MachineRole.INGRESS, MachineRole.BUILDER, MachineRole.WORKER].map((r) => (
              <CheckboxField
                key={r}
                checked={m.roles.includes(r)}
                disabled={setRoles.isPending}
                onCheckedChange={() => toggleRole(r)}
              >
                <span className="font-medium">{roleLabel[r]}</span>{' '}
                <span className="text-xs text-muted-foreground">{r === MachineRole.INGRESS ? 'runs Caddy, receives public traffic' : r === MachineRole.BUILDER ? 'may claim build leases' : 'runs service replicas'}</span>
              </CheckboxField>
            ))}
            <form className="flex gap-2 pt-2" onSubmit={(e) => { e.preventDefault(); rename.mutate(name); setName('') }}>
              <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Rename machine…" aria-label="New name" />
              <Button type="submit" size="sm" variant="secondary" className="h-9" disabled={!name}>Rename</Button>
            </form>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle className="flex items-center gap-2"><Waves className="size-4" />WireGuard</CardTitle></CardHeader>
          <CardContent className="space-y-1.5 text-sm">
            <div><span className="text-muted-foreground">Subnet</span> <code>{m.subnet}</code></div>
            <div><span className="text-muted-foreground">Public key</span> <code className="break-all text-xs">{m.publicKey}</code></div>
            <div><span className="text-muted-foreground">Endpoints</span> {m.endpoints.map((e) => <code key={e} className="mr-2">{e}</code>)}</div>
            <div className="flex flex-wrap gap-1.5 pt-1">{Object.entries(m.labels).map(([k, v]) => <Badge key={k}>{k}={v}</Badge>)}</div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Replicas ({reps.data?.length ?? 0})</CardTitle></CardHeader>
          <CardContent className="space-y-2">
            {!reps.data?.length && <p className="text-sm text-muted-foreground">Nothing scheduled here.</p>}
            {reps.data?.map((r) => (
              <div key={r.id} className="flex items-center gap-2 text-sm">
                <span className="flex-1 truncate">{svcName(r.serviceId)}</span>
                <ReplicaBadge state={r.state} />
                {r.state === ReplicaState.STRANDED && <span className="text-xs text-warning" title={r.message}>why?</span>}
              </div>
            ))}
          </CardContent>
        </Card>
      </div>
      <Dialog open={removing} onOpenChange={setRemoving}>
        <DialogContent>
          <DialogHeader title={`Remove ${m.name}?`} description="Replicas on this machine are rescheduled where possible. This cannot be undone." />
          <CheckboxField className="mb-4" checked={force} onCheckedChange={setForce}>Force (machine is unreachable)</CheckboxField>
          <div className="flex justify-end gap-2"><Button variant="ghost" onClick={() => setRemoving(false)}>Cancel</Button><Button variant="destructive" onClick={() => remove.mutate(undefined)} disabled={remove.isPending}>Remove machine</Button></div>
        </DialogContent>
      </Dialog>
    </Page>
  )
}
