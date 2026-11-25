import { createClient, ConnectError, Code, type Client, type Transport } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'
import { AuthService } from '@/gen/aetherix/v1/auth_pb'
import { ClusterService } from '@/gen/aetherix/v1/cluster_pb'
import { DeployService } from '@/gen/aetherix/v1/deploy_pb'
import { GitHubService } from '@/gen/aetherix/v1/github_pb'
import { LogService } from '@/gen/aetherix/v1/log_pb'
import { MachineService } from '@/gen/aetherix/v1/machine_pb'
import { ProjectService } from '@/gen/aetherix/v1/project_pb'
import { ServiceService } from '@/gen/aetherix/v1/service_pb'

export const isMock = import.meta.env.VITE_MOCK === '1'
export const apiBase: string = import.meta.env.VITE_API_BASE ?? ''

const TOKEN_KEY = 'ax.token'
export const tokenStore = {
  get: () => (typeof localStorage === 'undefined' ? null : localStorage.getItem(TOKEN_KEY)),
  set: (t: string) => localStorage.setItem(TOKEN_KEY, t),
  clear: () => localStorage.removeItem(TOKEN_KEY),
}

export type Clients = {
  auth: Client<typeof AuthService>
  cluster: Client<typeof ClusterService>
  deploy: Client<typeof DeployService>
  github: Client<typeof GitHubService>
  logs: Client<typeof LogService>
  machines: Client<typeof MachineService>
  projects: Client<typeof ProjectService>
  services: Client<typeof ServiceService>
}

function build(t: Transport): Clients {
  return {
    auth: createClient(AuthService, t),
    cluster: createClient(ClusterService, t),
    deploy: createClient(DeployService, t),
    github: createClient(GitHubService, t),
    logs: createClient(LogService, t),
    machines: createClient(MachineService, t),
    projects: createClient(ProjectService, t),
    services: createClient(ServiceService, t),
  }
}

let clients: Clients | undefined

/** Must be awaited once before rendering. The mock transport is code-split. */
export async function initApi(): Promise<void> {
  if (isMock) {
    const { createMockTransport } = await import('@/mock/transport')
    clients = build(createMockTransport())
    return
  }
  const transport = createConnectTransport({
    baseUrl: apiBase || window.location.origin,
    interceptors: [
      (next) => (req) => {
        const t = tokenStore.get()
        if (t) req.header.set('Authorization', `Bearer ${t}`)
        return next(req)
      },
    ],
  })
  clients = build(transport)
}

export const api = new Proxy({} as Clients, {
  get(_t, prop: keyof Clients) {
    if (!clients) throw new Error('initApi() has not completed')
    return clients[prop]
  },
})

/** Replace the API (tests). */
export function setApiForTests(c: Partial<Clients>) {
  clients = c as Clients
}

export function isUnimplemented(e: unknown): boolean {
  return ConnectError.from(e).code === Code.Unimplemented
}

export function isUnauthenticated(e: unknown): boolean {
  return ConnectError.from(e).code === Code.Unauthenticated
}

export function errMessage(e: unknown): string {
  return ConnectError.from(e).rawMessage || String(e)
}

/** Run a call; map Unimplemented to a fallback value (AuthService/GitHubService stubs). */
export async function orUnimplemented<T, F>(p: Promise<T>, fallback: F): Promise<T | F> {
  try {
    return await p
  } catch (e) {
    if (isUnimplemented(e)) return fallback
    throw e
  }
}
