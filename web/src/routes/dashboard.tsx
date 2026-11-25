import { Link, useNavigate } from '@tanstack/react-router'
import {
  Activity,
  ArrowUpCircle,
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
    <div className="ax-soft ax-row px-4 py-3.5">
      <div className="text-[11px] font-semibold uppercase tracking-[0.06em] text-muted-foreground">{label}</div>
      <div className={cn('mt-1.5 text-2xl font-semibold tracking-tight tabular-nums', color)}>{value}</div>
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
        hideTitle
        actions={
          <div className="flex flex-wrap gap-2 sm:hidden">
            <Button size="sm" variant="secondary" onClick={() => setOpen(true)}><Plus />New project</Button>
            <Button size="sm" variant="outline" asChild><Link to="/cluster/add"><Server className="size-3.5" />Add machine</Link></Button>
          </div>
        }
      />

      {status.isLoading ? (
        <ListSkeleton rows={2} />
      ) : status.error ? (
        <QueryError error={status.error} what="cluster status" />
      ) : (
        <div className="mb-5 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <Metric
            label="Machines"
            value={`${s?.machinesUp ?? 0}/${s?.machinesTotal ?? 0}`}
            hint={s?.machinesDown ? `${s.machinesDown} down` : (s?.machinesSuspect ? `${s.machinesSuspect} suspect` : 'all reporting')}
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

      <div className="mb-5 flex flex-wrap gap-2">
        <Link to="/projects" className="ax-pill"><FolderKanban className="size-3.5 text-primary" />Projects</Link>
        <button
          type="button"
          className="ax-pill cursor-pointer"
          onClick={() => {
            if (!firstProject) { setOpen(true); return }
            void nav({ to: '/projects/$projectId', params: { projectId: firstProject.id }, search: { new: 'github' } })
          }}
        >
          <Rocket className="size-3.5 text-primary" />New service
        </button>
        <Link to="/settings/updates" className="ax-pill">
          <ArrowUpCircle className="size-3.5 text-primary" />Updates
          {skew ? <Badge tone="warning">skew</Badge> : null}
        </Link>
        <Link to="/settings/backups" className="ax-pill"><Cloud className="size-3.5 text-primary" />Backups</Link>
        <Link to="/activity" className="ax-pill"><Activity className="size-3.5 text-primary" />Activity</Link>
      </div>

      <div className="grid gap-3.5 lg:grid-cols-[1.4fr_1fr]">
        <section className="space-y-3.5">
          <Card>
            <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
              <CardTitle>Projects</CardTitle>
              <Link to="/projects" className="text-sm text-muted-foreground hover:text-foreground">View all</Link>
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
                <ul className="divide-y divide-border/60">
                  {projects.data.slice(0, 6).map((p) => (
                    <li key={p.id}>
                      <Link
                        to="/projects/$projectId"
                        params={{ projectId: p.id }}
                        search={{}}
                        className="ax-row flex items-center gap-3 px-4 py-3 text-sm hover:bg-muted/40"
                      >
                        <div className="grid size-9 place-items-center rounded-[10px] bg-muted text-sm font-semibold text-primary">
                          {p.name.slice(0, 1).toUpperCase()}
                        </div>
                        <div className="min-w-0 flex-1">
                          <div className="truncate font-semibold tracking-tight">{p.name}</div>
                          <div className="truncate text-xs text-muted-foreground">{p.description || 'No description'}</div>
                        </div>
                        <div className="text-right text-xs">
                          <div className="font-medium tabular-nums">{serviceByProject.get(p.id) ?? 0} svc</div>
                          <div className="text-muted-foreground">created {timeAgo(p.createdAt)}</div>
                        </div>
                      </Link>
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
              <CardTitle>Machines</CardTitle>
              <Link to="/cluster" className="text-sm text-muted-foreground hover:text-foreground">Cluster</Link>
            </CardHeader>
            <CardContent className="p-3">
              {machines.isLoading ? (
                <ListSkeleton rows={2} />
              ) : machines.error ? (
                <QueryError error={machines.error} what="machines" />
              ) : !machines.data?.length ? (
                <Empty title="No machines">
                  <div className="mt-3"><Button size="sm" asChild><Link to="/cluster/add"><Plus />Add machine</Link></Button></div>
                </Empty>
              ) : (
                <div className="flex gap-2.5 overflow-x-auto">
                  {machines.data.map((m) => (
                    <Link
                      key={m.id}
                      to="/cluster/machines/$machineId"
                      params={{ machineId: m.id }}
                      className={cn(
                        'ax-soft ax-row min-w-[10.5rem] shrink-0 px-3.5 py-3 transition-colors hover:border-primary/35',
                        m.state === MachineState.DOWN && 'border-destructive/35',
                      )}
                    >
                      <div className="flex items-center gap-2">
                        <span className="truncate text-[13px] font-semibold">{m.name}</span>
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
            <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
              <CardTitle>Recent activity</CardTitle>
              <Link to="/activity" className="text-sm text-muted-foreground hover:text-foreground">Open feed</Link>
            </CardHeader>
            <CardContent className="p-0">
              {events.isLoading ? (
                <div className="p-4"><ListSkeleton rows={5} /></div>
              ) : events.error ? (
                <div className="p-4"><QueryError error={events.error} what="events" /></div>
              ) : !recent.length ? (
                <Empty title="Quiet so far">Deployments, machine state and replica changes will show up here.</Empty>
              ) : (
                <ul className="divide-y divide-border/60">
                  {recent.map((e) => (
                    <li key={e.id} className="ax-row flex gap-3 px-4 py-3 text-sm">
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
