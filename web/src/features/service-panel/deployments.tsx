import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { GitCommit, Hammer, RotateCcw, Rocket, Square, ChevronDown, ChevronRight, Play, Box, Server, Power } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Empty, ListSkeleton, QueryError } from '@/components/ui/states'
import { LogView } from '@/components/xterm-view'
import { DeploymentStatus, ReplicaState, type Deployment, type Service } from '@/gen/aetherix/v1/types_pb'
import { api, isUnimplemented } from '@/lib/api'
import { fetchServiceBuilds, type ServiceBuild } from '@/lib/extras'
import { keys, useAction, useDeployments } from '@/lib/queries'
import { DeployBadge, ReplicaBadge } from '@/lib/status'
import { cn, formatBytes, formatCpu, shortId, timeAgo, tsDate } from '@/lib/utils'

const inFlight = (s: DeploymentStatus) => s === DeploymentStatus.QUEUED || s === DeploymentStatus.BUILDING || s === DeploymentStatus.DEPLOYING

/** Newest created_at first (fallback: id). Matches API ListDeployments ordering. */
export function sortDeploymentsNewestFirst(deps: Deployment[]): Deployment[] {
  return [...deps].sort((a, b) => {
    const ta = tsDate(a.createdAt)?.getTime() ?? 0
    const tb = tsDate(b.createdAt)?.getTime() ?? 0
    if (tb !== ta) return tb - ta
    return a.id < b.id ? 1 : a.id > b.id ? -1 : 0
  })
}

export function duration(d: Deployment): string {
  const a = tsDate(d.createdAt)
  if (!a) return ''
  const b = tsDate(d.finishedAt) ?? new Date()
  const s = Math.max(0, Math.round((b.getTime() - a.getTime()) / 1000))
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`
}

export function DeploymentsTab({ service }: { service: Service }) {
  const q = useDeployments(service.id)
  const reps = useQuery({
    queryKey: ['replicas', 'service', service.id],
    queryFn: async () => (await api.services.listReplicas({ serviceId: service.id })).replicas,
    refetchInterval: 6000,
  })
  const inv = [keys.deployments(service.id), keys.service(service.id), ['replicas'], ['service'], ['builds', service.id]]
  const buildsQ = useQuery({
    queryKey: ['builds', service.id],
    queryFn: () => fetchServiceBuilds(service.id),
    refetchInterval: 4000,
  })
  const hasGithub = !!service.spec?.source?.github
  const hasImage = !!service.spec?.image
  const activeBuilds = (buildsQ.data ?? []).filter((b) => b.status === 'queued' || b.status === 'running' || b.status === 'failed').slice(0, 5)
  const redeploy = useAction(
    () => (hasGithub && !hasImage
      ? api.deploy.deploy({ serviceId: service.id })
      : api.deploy.deploy({ serviceId: service.id, redeploy: true })),
    { invalidate: inv, success: hasGithub && !hasImage ? 'Build queued' : 'Redeploy started' },
  )
  const simulate = useAction(() => api.github.simulatePush({ serviceId: service.id, ref: '' }), { invalidate: inv, success: 'Push simulated; build queued' })
  const scale = useAction((replicas: number) => api.services.scaleService({ serviceId: service.id, replicas }), {
    invalidate: inv,
    success: 'Scaled',
  })
  const global = service.spec?.mode === 2
  const replicasWanted = service.spec?.replicas ?? 0
  const running = replicasWanted > 0
  const prevKey = `ax:prev-replicas:${service.id}`
  const stop = () => {
    if (global) return
    if (replicasWanted > 0) sessionStorage.setItem(prevKey, String(replicasWanted))
    scale.mutate(0)
  }
  const start = () => {
    if (global) return
    const stored = Number(sessionStorage.getItem(prevKey) || '')
    scale.mutate(stored > 0 ? stored : 1)
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap gap-2">
        <Button size="sm" onClick={() => redeploy.mutate(undefined)} disabled={redeploy.isPending || (!running && !(hasGithub && !hasImage))}><Rocket />{hasGithub && !hasImage ? 'Build & deploy' : 'Redeploy'}</Button>
        {!global && (
          <>
            <Button size="sm" variant="secondary" disabled={!running || scale.isPending} onClick={stop}><Power />Stop app</Button>
            <Button size="sm" variant="secondary" disabled={running || scale.isPending} onClick={start}><Play />Start app</Button>
          </>
        )}
        {hasGithub && <Button size="sm" variant="outline" onClick={() => simulate.mutate(undefined)} disabled={simulate.isPending} title="Fetch the latest commit and run the real pipeline"><Play />Simulate push</Button>}
      </div>
      {!global && (
        <p className="text-xs text-muted-foreground">
          Stop scales to 0 (brief downtime). Start restores the previous replica count{replicasWanted === 0 ? ' (app is stopped)' : ''}.
        </p>
      )}
      {simulate.error && isUnimplemented(simulate.error) && <p className="text-xs text-muted-foreground">GitHub integration isn’t available on this node yet.</p>}
      {!!activeBuilds.length && (
        <div className="space-y-2">
          <div className="text-xs font-medium text-muted-foreground">Builds</div>
          <ul className="space-y-2">
            {activeBuilds.map((b) => <BuildRow key={b.id} b={b} />)}
          </ul>
        </div>
      )}
      {q.isLoading ? <ListSkeleton /> : q.error ? <QueryError error={q.error} what="deployments" /> : !q.data?.length ? (
        <Card><Empty title="No deployments yet">{hasGithub ? 'A build must finish before the first deployment appears.' : 'Deploy this service to see its history here.'}</Empty></Card>
      ) : (
        <ol className="relative space-y-3 before:absolute before:bottom-2 before:left-[11px] before:top-2 before:w-px before:bg-border" aria-label="Deployment timeline">
          {sortDeploymentsNewestFirst(q.data).map((d, i) => {
            const active = d.id === service.activeDeploymentId
            return (
              <DeploymentItem
                key={d.id}
                d={d}
                service={service}
                replicas={(reps.data ?? []).filter((r) => r.deploymentId === d.id)}
                defaultOpen={i === 0 || active}
              />
            )
          })}
        </ol>
      )}
    </div>
  )
}

function BuildRow({ b }: { b: ServiceBuild }) {
  const tone = b.status === 'failed' ? 'danger' : b.status === 'running' ? 'info' : b.status === 'succeeded' ? 'success' : 'neutral'
  return (
    <Card className="px-3 py-2">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Hammer className="size-3.5 text-muted-foreground" />
        <Badge tone={tone}>{b.status}</Badge>
        <span className="font-mono text-xs text-muted-foreground">{shortId(b.id)}</span>
        {b.commit_sha && <span className="font-mono text-xs">{b.commit_sha.slice(0, 7)}</span>}
        {b.created_at && <span className="text-xs text-muted-foreground">{timeAgo(new Date(b.created_at))}</span>}
      </div>
      {b.status === 'failed' && <p className="mt-1 text-xs text-muted-foreground">Open Activity for details, or retry with Simulate push.</p>}
      {b.status === 'queued' && <p className="mt-1 text-xs text-muted-foreground">Waiting for a builder node to pick this up…</p>}
    </Card>
  )
}

function DeploymentItem({ d, service, replicas, defaultOpen }: { d: Deployment; service: Service; replicas: { id: string; state: ReplicaState; machineId: string; message?: string; startedAt?: unknown }[]; defaultOpen: boolean }) {
  const [open, setOpen] = useState(defaultOpen)
  const active = d.id === service.activeDeploymentId
  const inv = [keys.deployments(service.id), ['service']]
  const rollback = useAction(() => api.deploy.rollback({ serviceId: service.id, deploymentId: d.id }), { invalidate: inv, success: `Rolling back to ${d.commitSha || shortId(d.id)}` })
  const cancel = useAction(() => api.deploy.cancelDeployment({ deploymentId: d.id }), { invalidate: inv, success: 'Deployment cancelled' })
  const canRollback = !active && !inFlight(d.status) && d.status !== DeploymentStatus.FAILED
  const image = d.spec?.image || d.spec?.source?.image || service.spec?.image || ''
  const res = d.spec?.resources
  return (
    <li className="relative pl-8">
      <span className={cn('absolute left-1 top-3 size-[15px] rounded-full border-2 bg-card', active ? 'border-success' : inFlight(d.status) ? 'border-primary' : d.status === DeploymentStatus.FAILED ? 'border-destructive' : 'border-muted-foreground')} />
      <Card>
        <button className="flex w-full cursor-pointer items-center gap-3 p-3 text-left" onClick={() => setOpen(!open)} aria-expanded={open}>
          {open ? <ChevronDown className="size-4 shrink-0" /> : <ChevronRight className="size-4 shrink-0" />}
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <DeployBadge status={d.status} />
              {active && <Badge tone="success">current</Badge>}
            </div>
            <div className="mt-1 flex items-center gap-1.5 truncate text-sm"><GitCommit className="size-3.5 shrink-0 text-muted-foreground" /><code className="text-xs">{d.commitSha || shortId(d.id)}</code><span className="truncate">{d.commitMessage || (image ? `Image ${image}` : 'Manual deploy')}</span></div>
            <div className="mt-0.5 text-xs text-muted-foreground">{d.author && `${d.author} · `}{timeAgo(d.createdAt)} · {duration(d)}</div>
          </div>
        </button>
        {open && (
          <div className="space-y-3 border-t p-3">
            {d.statusMessage && <p className="text-sm text-muted-foreground">{d.statusMessage}</p>}

            <dl className="grid gap-2 text-sm sm:grid-cols-2">
              <div>
                <dt className="text-[11px] uppercase tracking-wide text-muted-foreground">Image</dt>
                <dd className="flex items-center gap-1.5 truncate font-mono text-xs"><Box className="size-3.5 shrink-0 text-muted-foreground" />{image || '—'}</dd>
              </div>
              {d.imageDigest && (
                <div>
                  <dt className="text-[11px] uppercase tracking-wide text-muted-foreground">Digest</dt>
                  <dd className="truncate font-mono text-xs">{d.imageDigest}</dd>
                </div>
              )}
              <div>
                <dt className="text-[11px] uppercase tracking-wide text-muted-foreground">Replicas</dt>
                <dd>{d.spec?.replicas ?? service.spec?.replicas ?? '—'}</dd>
              </div>
              <div>
                <dt className="text-[11px] uppercase tracking-wide text-muted-foreground">Resources</dt>
                <dd className="text-xs text-muted-foreground">
                  {res?.cpuMillis ? formatCpu(res.cpuMillis) : '—'} · {res?.memoryBytes ? formatBytes(res.memoryBytes) : '—'}
                </dd>
              </div>
              {d.buildId && (
                <div>
                  <dt className="text-[11px] uppercase tracking-wide text-muted-foreground">Build</dt>
                  <dd className="font-mono text-xs">{d.buildId}</dd>
                </div>
              )}
              {d.previousDeploymentId && (
                <div>
                  <dt className="text-[11px] uppercase tracking-wide text-muted-foreground">Previous</dt>
                  <dd className="font-mono text-xs">{shortId(d.previousDeploymentId)}</dd>
                </div>
              )}
            </dl>

            {!!replicas.length && (
              <div>
                <div className="mb-1.5 flex items-center gap-1.5 text-xs font-medium text-muted-foreground"><Server className="size-3.5" />Replicas on this deployment</div>
                <ul className="space-y-1">
                  {replicas.map((r) => {
                    const unhealthy = /health check fail/i.test(r.message || '')
                    return (
                      <li key={r.id} className={cn('flex flex-wrap items-center gap-2 rounded-md px-1.5 py-1 text-sm', unhealthy && 'bg-destructive/10')}>
                        <ReplicaBadge state={r.state} message={r.message} />
                        <code className="text-xs text-muted-foreground">{shortId(r.id)}</code>
                        <span className="text-xs text-muted-foreground">on {shortId(r.machineId)}</span>
                        {r.message && (
                          <span className={cn('truncate text-xs', unhealthy ? 'font-medium text-destructive' : 'text-muted-foreground')}>
                            {r.message}
                          </span>
                        )}
                      </li>
                    )
                  })}
                </ul>
              </div>
            )}
            {!replicas.length && active && (
              <p className="text-xs text-muted-foreground">No replicas currently assigned to this deployment.</p>
            )}

            <div className="flex flex-wrap gap-2">
              {canRollback && <Button size="sm" variant="secondary" onClick={() => rollback.mutate(undefined)} disabled={rollback.isPending}><RotateCcw />Rollback to this</Button>}
              {inFlight(d.status) && <Button size="sm" variant="outline" onClick={() => cancel.mutate(undefined)}><Square />Cancel</Button>}
              {active && !inFlight(d.status) && <Badge tone="success">Serving traffic</Badge>}
            </div>

            {(d.buildId || inFlight(d.status)) && (
              <div>
                <div className="mb-1.5 flex items-center gap-1.5 text-xs font-medium text-muted-foreground"><Hammer className="size-3.5" />Build logs{inFlight(d.status) && ' (live)'}</div>
                <BuildLogs d={d} />
              </div>
            )}
          </div>
        )}
      </Card>
    </li>
  )
}

function BuildLogs({ d }: { d: Deployment }) {
  return (
    <LogView
      className="h-56 w-full overflow-hidden rounded-md border p-2"
      deps={[d.id, d.buildId]}
      open={async (emit, signal) => {
        if (!d.buildId) return emit('\x1b[2mNo build record for this deployment (image deploy).\x1b[0m')
        try {
          for await (const e of api.logs.streamBuildLogs({ buildId: d.buildId, follow: inFlight(d.status) }, { signal })) emit(e.line, e.stream)
        } catch (e) {
          if (isUnimplemented(e)) emit('\x1b[2mBuild logs are not available on this node yet.\x1b[0m')
          else throw e
        }
      }}
    />
  )
}
