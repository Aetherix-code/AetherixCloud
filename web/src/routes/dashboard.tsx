import { Link, useNavigate } from '@tanstack/react-router'
import {
  Activity,
  ArrowUpCircle,
  Boxes,
  Cloud,
  FolderKanban,
  LayoutDashboard,
  Plus,
  Rocket,
  Server,
} from 'lucide-react'
import { Page, PageHeader } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, ListSkeleton, QueryError } from '@/components/ui/states'
import { MachineState } from '@/gen/aetherix/v1/types_pb'
import { MachineBadge } from '@/lib/status'
import { keys, useAction, useEvents, useMachines, useProjects, useServices, useStatus } from '@/lib/queries'
import { api } from '@/lib/api'
import { cn, timeAgo } from '@/lib/utils'
import { useMemo, useState } from 'react'
import { Dialog, DialogContent, DialogHeader } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'

function Metric({ label, value, hint, tone }: { label: string; value: string; hint?: string; tone?: 'ok' | 'warn' | 'bad' | 'neutral' }) {
  const color =
    tone === 'ok' ? 'text-success' : tone === 'warn' ? 'text-warning' : tone === 'bad' ? 'text-destructive' : 'text-foreground'
  return (
    <div className="ax-row min-w-0 rounded-lg border bg-card/80 px-3.5 py-3">
      <div className="text-[11px] font-semibold uppercase tracking-[0.06em] text-muted-foreground">{label}</div>
      <div className={cn('mt-1 text-2xl font-semibold tracking-tight tabular-nums', color)}>{value}</div>
      {hint && <div className="mt-1 truncate text-xs text-muted-foreground">{hint}</div>}
    </div>
  )
}

export function DashboardPage() {
  const status = useStatus()
  const machines = useMachines()
  const projects = useProjects()
  const services = useServices()
  const events = useEvents()
  const nav = useNavigate()
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  const create = useAction((a: { name: string; description: string }) => api.projects.createProject(a), {
    invalidate: [keys.projects],
    success: 'Project created',
    onSuccess: (r) => {
      setOpen(false)
      setName('')
      setDesc('')
      if (r.project) void nav({ to: '/projects/$projectId', params: { projectId: r.project.id }, search: {} })
    },
  })

  const s = status.data
  const serviceByProject = useMemo(() => {
    const m = new Map<string, number>()
    for (const svc of services.data ?? []) m.set(svc.projectId, (m.get(svc.projectId) ?? 0) + 1)
    return m
  }, [services.data])

  const versions = useMemo(() => {
    const set = new Set((machines.data ?? []).map((m) => m.agentVersion?.replace(/^v/, '') || 'unknown').filter(Boolean))
    return [...set]
  }, [machines.data])
  const skew = versions.length > 1

  const recent = (events.data ?? []).slice(0, 8)
  const firstProject = projects.data?.[0]

  return (
    <Page wide>
      <PageHeader
        title="Dashboard"
        description={s?.cluster ? `${s.cluster.name} · ${s.cluster.network}${s.servedBy ? ` · via ${s.servedBy}` : ''}` : 'Cluster overview'}
        actions={
          <div className="flex flex-wrap gap-2">
            <Button variant="secondary" onClick={() => setOpen(true)}><Plus />New project</Button>
            <Button asChild variant="outline"><Link to="/cluster/add"><Server className="size-4" />Add machine</Link></Button>
          </div>
        }
      />

      {status.isLoading ? (
        <ListSkeleton rows={2} />
      ) : status.error ? (
        <QueryError error={status.error} what="cluster status" />
      ) : (
        <div className="mb-6 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <Metric
            label="Machines"
            value={`${s?.machinesUp ?? 0}/${s?.machinesTotal ?? 0}`}
            hint={s?.machinesDown ? `${s.machinesDown} down` : 'all reporting'}
            tone={(s?.machinesDown ?? 0) > 0 ? 'bad' : (s?.machinesSuspect ?? 0) > 0 ? 'warn' : 'ok'}
          />
          <Metric
            label="Replicas"
            value={`${s?.replicasRunning ?? 0}/${s?.replicasDesired ?? 0}`}
            hint="running / desired"
            tone={(s?.replicasRunning ?? 0) < (s?.replicasDesired ?? 0) ? 'warn' : 'ok'}
          />
          <Metric
            label="Services"
            value={String(s?.servicesTotal ?? services.data?.length ?? 0)}
            hint={`${projects.data?.length ?? 0} projects`}
            tone="neutral"
          />
          <Metric
            label="Versions"
            value={skew ? `${versions.length} skew` : (versions[0] ? `v${versions[0]}` : (s?.version ?? '—'))}
            hint={skew ? versions.map((v) => `v${v}`).join(' · ') : 'cluster aligned'}
            tone={skew ? 'warn' : 'ok'}
          />
        </div>
      )}

      <div className="mb-6 flex flex-wrap gap-2">
        <Button size="sm" variant="outline" asChild><Link to="/projects"><FolderKanban className="size-3.5" />Projects</Link></Button>
        <Button
          size="sm"
          variant="outline"
          onClick={() => {
            if (!firstProject) { setOpen(true); return }
            void nav({ to: '/projects/$projectId', params: { projectId: firstProject.id }, search: { new: 'github' } })
          }}
        >
          <Rocket className="size-3.5" />New service
        </Button>
        <Button size="sm" variant="outline" asChild><Link to="/settings/updates"><ArrowUpCircle className="size-3.5" />Updates{skew ? <Badge tone="warning">skew</Badge> : null}</Link></Button>
        <Button size="sm" variant="outline" asChild><Link to="/settings/backups"><Cloud className="size-3.5" />Backups</Link></Button>
        <Button size="sm" variant="outline" asChild><Link to="/activity"><Activity className="size-3.5" />Activity</Link></Button>
      </div>

      <div className="grid gap-4 lg:grid-cols-[1.4fr_1fr]">
        <section className="space-y-4">
          <Card>
            <CardHeader className="flex-row items-center justify-between gap-2">
              <div>
                <CardTitle className="flex items-center gap-2"><FolderKanban className="size-4 opacity-80" />Projects</CardTitle>
              </div>
              <Button size="sm" variant="ghost" asChild><Link to="/projects">View all</Link></Button>
            </CardHeader>
            <CardContent className="p-0">
              {projects.isLoading || services.isLoading ? (
                <div className="p-4"><ListSkeleton rows={3} /></div>
              ) : projects.error ? (
                <div className="p-4"><QueryError error={projects.error} what="projects" /></div>
              ) : !projects.data?.length ? (
                <Empty title="No projects yet" icon={<LayoutDashboard className="size-7" />}>
                  <div className="mt-3"><Button size="sm" onClick={() => setOpen(true)}><Plus />Create a project</Button></div>
                </Empty>
              ) : (
                <ul className="divide-y">
                  {projects.data.slice(0, 6).map((p) => (
                    <li key={p.id}>
                      <Link
                        to="/projects/$projectId"
                        params={{ projectId: p.id }}
                        search={{}}
                        className="ax-row flex items-center gap-3 px-4 py-3 text-sm hover:bg-muted/50"
                      >
                        <div className="grid size-8 place-items-center rounded-md bg-primary/10 text-primary">
                          <Boxes className="size-4" />
                        </div>
                        <div className="min-w-0 flex-1">
                          <div className="truncate font-medium tracking-tight">{p.name}</div>
                          <div className="truncate text-xs text-muted-foreground">{p.description || 'No description'}</div>
                        </div>
                        <div className="text-right text-xs text-muted-foreground">
                          <div className="font-medium text-foreground tabular-nums">{serviceByProject.get(p.id) ?? 0} services</div>
                          <div>created {timeAgo(p.createdAt)}</div>
                        </div>
                      </Link>
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="flex-row items-center justify-between gap-2">
              <CardTitle className="flex items-center gap-2"><Server className="size-4 opacity-80" />Machines</CardTitle>
              <Button size="sm" variant="ghost" asChild><Link to="/cluster">Cluster</Link></Button>
            </CardHeader>
            <CardContent className="p-0">
              {machines.isLoading ? (
                <div className="p-4"><ListSkeleton rows={2} /></div>
              ) : machines.error ? (
                <div className="p-4"><QueryError error={machines.error} what="machines" /></div>
              ) : !machines.data?.length ? (
                <Empty title="No machines">
                  <div className="mt-3"><Button size="sm" asChild><Link to="/cluster/add"><Plus />Add machine</Link></Button></div>
                </Empty>
              ) : (
                <div className="flex gap-2 overflow-x-auto p-3">
                  {machines.data.map((m) => (
                    <Link
                      key={m.id}
                      to="/cluster/machines/$machineId"
                      params={{ machineId: m.id }}
                      className={cn(
                        'ax-row min-w-[11.5rem] shrink-0 rounded-md border px-3 py-2.5 transition-colors hover:border-primary/40',
                        m.state === MachineState.DOWN && 'border-destructive/35',
                      )}
                    >
                      <div className="flex items-center gap-2">
                        <span className="truncate text-sm font-medium">{m.name}</span>
                        <MachineBadge state={m.state} />
                      </div>
                      <div className="mt-1.5 font-mono text-[11px] text-muted-foreground">
                        {m.agentVersion ? `v${m.agentVersion.replace(/^v/, '')}` : '—'}
                        {m.cordoned ? ' · cordoned' : ''}
                      </div>
                      <div className="mt-0.5 text-[11px] text-muted-foreground">seen {timeAgo(m.lastSeen)}</div>
                    </Link>
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </section>

        <section>
          <Card className="h-full">
            <CardHeader className="flex-row items-center justify-between gap-2">
              <CardTitle className="flex items-center gap-2"><Activity className="size-4 opacity-80" />Recent activity</CardTitle>
              <Button size="sm" variant="ghost" asChild><Link to="/activity">Open feed</Link></Button>
            </CardHeader>
            <CardContent className="p-0">
              {events.isLoading ? (
                <div className="p-4"><ListSkeleton rows={5} /></div>
              ) : events.error ? (
                <div className="p-4"><QueryError error={events.error} what="events" /></div>
              ) : !recent.length ? (
                <Empty title="Quiet so far">Deployments, machine state and replica changes will show up here.</Empty>
              ) : (
                <ul className="divide-y">
                  {recent.map((e) => (
                    <li key={e.id} className="ax-row flex gap-3 px-4 py-2.5 text-sm">
                      <div className="min-w-0 flex-1">
                        <div className="leading-snug">{e.message}</div>
                        <code className="text-[11px] text-muted-foreground">{e.type}</code>
                      </div>
                      <span className="shrink-0 text-xs text-muted-foreground tabular-nums">{timeAgo(e.time)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>
        </section>
      </div>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader title="New project" description="A production environment is created automatically." />
          <form className="space-y-4" onSubmit={(e) => { e.preventDefault(); create.mutate({ name, description: desc }) }}>
            <Field label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} required autoFocus pattern="[a-z0-9][a-z0-9-]*" title="lowercase letters, digits and dashes" /></Field>
            <Field label="Description"><Input value={desc} onChange={(e) => setDesc(e.target.value)} /></Field>
            <div className="flex justify-end"><Button type="submit" disabled={create.isPending || !name}>Create</Button></div>
          </form>
        </DialogContent>
      </Dialog>
    </Page>
  )
}
