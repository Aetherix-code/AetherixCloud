import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { getRouteApi, useNavigate } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Empty, QueryError } from '@/components/ui/states'
import { ProjectCanvas } from '@/features/canvas'
import { NewServiceDialog } from '@/features/new-service'
import { ServicePanel } from '@/features/service-panel'
import { api } from '@/lib/api'
import { refEdges } from '@/lib/refs'
import { useProject } from '@/lib/queries'

const route = getRouteApi('/app/projects/$projectId')

/** Prefer the sole non-ephemeral environment; environments are an internal detail (no UI). */
function defaultEnvId(envs: { id: string; ephemeral: boolean }[]): string {
  return (envs.find((e) => !e.ephemeral) ?? envs[0])?.id ?? ''
}

export function ProjectPage() {
  const { projectId } = route.useParams()
  const search = route.useSearch()
  const nav = useNavigate()
  const q = useProject(projectId)
  const envs = q.data?.environments ?? []
  const envId = defaultEnvId(envs)
  // All project services (ignore ephemeral preview leftovers if any remain in store).
  const services = useMemo(
    () => (q.data?.services ?? []).filter((s) => !s.environmentId || s.environmentId === envId || !envs.find((e) => e.id === s.environmentId)?.ephemeral),
    [q.data, envId, envs],
  )

  const vars = useQuery({
    queryKey: ['variables', envId, '*'],
    enabled: !!envId,
    queryFn: async () => (await api.projects.listVariables({ environmentId: envId, serviceId: '*' })).variables,
  })
  const reps = useQuery({ queryKey: ['project', 'replicas', projectId], queryFn: async () => (await api.services.listReplicas({})).replicas, refetchInterval: 6000 })
  const doms = useQuery({
    queryKey: ['project', 'domains', projectId, services.map((s) => s.id).join()],
    enabled: services.length > 0,
    queryFn: async () => (await Promise.all(services.map((s) => api.services.listDomains({ serviceId: s.id })))).flatMap((r) => r.domains),
  })

  const edges = useMemo(() => {
    const names = new Map(services.map((s) => [s.id, s.name]))
    return refEdges(
      (vars.data ?? []).filter((v) => names.has(v.serviceId)).map((v) => ({ serviceName: names.get(v.serviceId)!, value: v.value })),
      new Set(services.map((s) => s.name)),
    )
  }, [vars.data, services])

  const setSearch = (patch: Record<string, string | undefined>) =>
    void nav({ to: '/projects/$projectId', params: { projectId }, search: (prev: Record<string, unknown>) => ({ ...prev, ...patch }) as never })

  if (q.isLoading) return <div className="p-6"><Skeleton className="h-[60vh] w-full" /></div>
  if (q.error) return <div className="p-6"><QueryError error={q.error} what="project" /></div>
  const selected = services.find((s) => s.id === search.service)

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-3 border-b px-4 py-2.5">
        <h1 className="text-base font-semibold tracking-tight">{q.data?.project?.name}</h1>
        <Button size="sm" className="ml-auto" onClick={() => setSearch({ new: 'github' })}><Plus />New</Button>
      </div>
      <div className="flex min-h-0 flex-1">
        <div className="dot-grid relative min-w-0 flex-1">
          {services.length === 0 ? (
            <Empty title="No services yet">
              Press <kbd className="rounded border bg-muted px-1">⌘K</kbd> or “New” to deploy from GitHub, an image or a template.
              Database templates attach persistent storage automatically; add TCP ports under Settings for external DB access.
            </Empty>
          ) : (
            <ProjectCanvas
              projectId={projectId}
              environmentId={envId}
              services={services}
              replicas={reps.data ?? []}
              domains={doms.data ?? []}
              edges={edges}
              variables={vars.data ?? []}
              selected={search.service}
              onSelect={(id) => setSearch({ service: id, tab: undefined })}
            />
          )}
        </div>
        {selected && envId && (
          <ServicePanel key={selected.id} service={selected} environmentId={envId} services={services} tab={search.tab ?? 'deployments'} onTab={(tab) => setSearch({ tab })} onClose={() => setSearch({ service: undefined, tab: undefined })} />
        )}
      </div>
      {search.new && (
        <NewServiceDialog
          projectId={projectId}
          environment={envId}
          kind={search.new}
          template={search.template}
          onClose={() => setSearch({ new: undefined, template: undefined })}
          onCreated={(id) => setSearch({ new: undefined, template: undefined, service: id })}
        />
      )}
    </div>
  )
}
