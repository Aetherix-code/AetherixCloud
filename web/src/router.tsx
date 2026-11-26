import { createRootRoute, createRoute, createRouter, Navigate, Outlet, redirect } from '@tanstack/react-router'
import { AppLayout } from '@/components/layout'
import { useAuth } from '@/lib/auth'
import { Skeleton } from '@/components/ui/skeleton'
import { LoginPage } from '@/routes/login'
import { DashboardPage } from '@/routes/dashboard'
import { ProjectsPage } from '@/routes/projects'
import { ProjectPage } from '@/routes/project'
import { ClusterPage, MachinePage } from '@/routes/cluster'
import { AddMachinePage } from '@/routes/add-machine'
import { ActivityPage } from '@/routes/activity'
import { AuditSettings } from '@/routes/audit'
import { ConsentPage } from '@/routes/consent'
import { SetupPage } from '@/routes/setup'
import { useSetupStatus } from '@/lib/queries'
import { AgentsSettings, BackupsSettings, GeneralSettings, GithubSettings, S3TargetsSettings, UpdatesSettings, UsersSettings } from '@/routes/settings'

const rootRoute = createRootRoute({ component: Outlet })

export interface LoginSearch {
  redirect?: string
}
const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  component: LoginPage,
  validateSearch: (s: Record<string, unknown>): LoginSearch => ({ redirect: typeof s.redirect === 'string' ? s.redirect : undefined }),
})
const setupRoute = createRoute({ getParentRoute: () => rootRoute, path: '/setup', component: SetupPage })
const consentRoute = createRoute({ getParentRoute: () => rootRoute, path: '/oauth/consent', component: ConsentPage })

function Guard() {
  const { status } = useAuth()
  const setup = useSetupStatus()
  if (status === 'loading' || (status === 'anon' && setup.isLoading))
    return (
      <div className="p-8">
        <Skeleton className="h-8 w-48" />
      </div>
    )
  if (status === 'anon') {
    if (setup.data) return <Navigate to="/setup" />
    const back = `${window.location.pathname}${window.location.search}`
    return <Navigate to="/login" search={{ redirect: back === '/' ? undefined : back }} />
  }
  return <AppLayout />
}
const appRoute = createRoute({ getParentRoute: () => rootRoute, id: 'app', component: Guard })

const indexRoute = createRoute({ getParentRoute: () => appRoute, path: '/', component: DashboardPage })
const projectsRoute = createRoute({ getParentRoute: () => appRoute, path: '/projects', component: ProjectsPage })

export interface ProjectSearch {
  service?: string
  tab?: string
  new?: string
  template?: string
}
const projectRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/projects/$projectId',
  component: ProjectPage,
  validateSearch: (s: Record<string, unknown>): ProjectSearch => ({
    service: typeof s.service === 'string' ? s.service : undefined,
    tab: typeof s.tab === 'string' ? s.tab : undefined,
    new: typeof s.new === 'string' ? s.new : undefined,
    template: typeof s.template === 'string' ? s.template : undefined,
  }),
})

const clusterRoute = createRoute({ getParentRoute: () => appRoute, path: '/cluster', component: ClusterPage })
const addMachineRoute = createRoute({ getParentRoute: () => appRoute, path: '/cluster/add', component: AddMachinePage })
const machineRoute = createRoute({ getParentRoute: () => appRoute, path: '/cluster/machines/$machineId', component: MachinePage })
const activityRoute = createRoute({ getParentRoute: () => appRoute, path: '/activity', component: ActivityPage })

const settingsIndex = createRoute({ getParentRoute: () => appRoute, path: '/settings', beforeLoad: () => { throw redirect({ to: '/settings/users' }) } })
const settingsUsers = createRoute({ getParentRoute: () => appRoute, path: '/settings/users', component: UsersSettings })
const settingsGithub = createRoute({ getParentRoute: () => appRoute, path: '/settings/github', component: GithubSettings })
const settingsAgents = createRoute({ getParentRoute: () => appRoute, path: '/settings/agents', component: AgentsSettings })
const settingsAudit = createRoute({ getParentRoute: () => appRoute, path: '/settings/audit', component: AuditSettings })
const settingsBackups = createRoute({ getParentRoute: () => appRoute, path: '/settings/backups', component: BackupsSettings })
const settingsS3Targets = createRoute({ getParentRoute: () => appRoute, path: '/settings/s3-targets', component: S3TargetsSettings })
const settingsUpdates = createRoute({ getParentRoute: () => appRoute, path: '/settings/updates', component: UpdatesSettings })
const settingsGeneral = createRoute({ getParentRoute: () => appRoute, path: '/settings/general', component: GeneralSettings })

const routeTree = rootRoute.addChildren([
  loginRoute,
  setupRoute,
  consentRoute,
  appRoute.addChildren([
    indexRoute,
    projectsRoute,
    projectRoute,
    clusterRoute,
    addMachineRoute,
    machineRoute,
    activityRoute,
    settingsIndex,
    settingsUsers,
    settingsGithub,
    settingsAgents,
    settingsAudit,
    settingsS3Targets,
    settingsBackups,
    settingsUpdates,
    settingsGeneral,
  ]),
])

export const router = createRouter({ routeTree, defaultPreload: 'intent' })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
