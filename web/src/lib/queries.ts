import { useMutation, useQuery, useQueryClient, type QueryKey } from '@tanstack/react-query'
import { api, errMessage, orUnimplemented } from './api'
import { toast } from '@/components/ui/toast'

/** `stream.*` events are high-volume plumbing (log/exec streams); never shown. */
export const isStreamEvent = (type: string) => type.startsWith('stream.')

export const keys = {
  status: ['status'] as const,
  machines: ['machines'] as const,
  projects: ['projects'] as const,
  project: (id: string) => ['project', id] as const,
  service: (id: string) => ['service', id] as const,
  deployments: (sid: string) => ['deployments', sid] as const,
  variables: (env: string, sid: string) => ['variables', env, sid] as const,
  events: ['events'] as const,
  users: ['users'] as const,
  tokens: ['tokens'] as const,
  grants: ['grants'] as const,
  ghApp: ['github', 'app'] as const,
  installations: ['github', 'installations'] as const,
}

export const useStatus = () => useQuery({ queryKey: keys.status, queryFn: () => api.cluster.getStatus({}), refetchInterval: 15000 })
export const useMachines = () => useQuery({ queryKey: keys.machines, queryFn: async () => (await api.machines.listMachines({})).machines, refetchInterval: 10000 })
export const useProjects = () => useQuery({ queryKey: keys.projects, queryFn: async () => (await api.projects.listProjects({})).projects })
export const useProject = (id: string) => useQuery({ queryKey: keys.project(id), queryFn: () => api.projects.getProject({ project: id }) })
export const useServices = (projectId?: string) =>
  useQuery({
    queryKey: projectId ? (['services', projectId] as const) : (['services', 'all'] as const),
    queryFn: async () => (await api.services.listServices(projectId ? { project: projectId } : {})).services,
  })
export const useService = (id?: string) =>
  useQuery({ queryKey: keys.service(id ?? ''), enabled: !!id, queryFn: () => api.services.getService({ service: id! }), refetchInterval: 8000 })
export const useDeployments = (sid: string) =>
  useQuery({ queryKey: keys.deployments(sid), queryFn: async () => (await api.deploy.listDeployments({ serviceId: sid, limit: 50 })).deployments, refetchInterval: 5000 })
export const useVariables = (env: string, sid: string) =>
  useQuery({ queryKey: keys.variables(env, sid), enabled: !!env, queryFn: async () => (await api.projects.listVariables({ environmentId: env, serviceId: sid })).variables })
export const useEvents = () => useQuery({ queryKey: keys.events, queryFn: async () => (await api.cluster.listEvents({ limit: 200 })).events.filter((e) => !isStreamEvent(e.type)) })
export const useSetupStatus = () => useQuery({ queryKey: ['setup'], retry: false, queryFn: () => orUnimplemented(api.auth.getSetupStatus({}).then((r) => r.needsSetup), false) })
export const useGrants = () => useQuery({ queryKey: keys.grants, retry: false, queryFn: () => orUnimplemented(api.auth.listOAuthGrants({}).then((r) => r.grants), null) })
export const useAudit = (actionPrefix = '', actorUserId = '') =>
  useQuery({
    queryKey: ['audit', actionPrefix, actorUserId],
    retry: false,
    refetchInterval: 15000,
    queryFn: () => orUnimplemented(api.auth.listAuditLog({ limit: 200, actionPrefix, actorUserId }).then((r) => r.entries), null),
  })
export const useUsers = () => useQuery({ queryKey: keys.users, retry: false, queryFn: () => orUnimplemented(api.auth.listUsers({}).then((r) => r.users), null) })
export const useTokens = () => useQuery({ queryKey: keys.tokens, retry: false, queryFn: () => orUnimplemented(api.auth.listApiTokens({}).then((r) => r.tokens), null) })
export const useGithubApp = () => useQuery({ queryKey: keys.ghApp, retry: false, queryFn: () => orUnimplemented(api.github.getApp({}), null) })

/** Mutation that toasts failures and invalidates the given query keys on success. */
export function useAction<A, R>(fn: (arg: A) => Promise<R>, opts: { invalidate?: QueryKey[]; success?: string | ((r: R) => string); onSuccess?: (r: R) => void } = {}) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: fn,
    onSuccess: (r) => {
      for (const k of opts.invalidate ?? []) void qc.invalidateQueries({ queryKey: k })
      if (opts.success) toast.success(typeof opts.success === 'function' ? opts.success(r) : opts.success)
      opts.onSuccess?.(r)
    },
    onError: (e) => toast.error(errMessage(e)),
  })
}
