import { Link, Outlet, useNavigate, useRouterState } from '@tanstack/react-router'
import {
  Activity,
  ArrowUpCircle,
  Bot,
  Cloud,
  Database,
  FolderKanban,
  GitBranch,
  LayoutDashboard,
  LogOut,
  Menu,
  Moon,
  Plus,
  ScrollText,
  Search,
  Server,
  Settings,
  Sun,
  Users,
  X,
} from 'lucide-react'
import { useState, type ComponentType, type ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { CommandPalette, openPaletteEvent } from '@/components/command-palette'
import { useAuth } from '@/lib/auth'
import { useLiveEvents } from '@/lib/live'
import { isMock } from '@/lib/api'
import { useMachines, useStatus } from '@/lib/queries'
import { useSidebarCollapsed } from '@/lib/sidebar'
import { useTheme } from '@/lib/theme'
import { cn } from '@/lib/utils'

type NavLink = {
  to: string
  label: string
  short?: string
  icon: ComponentType<{ className?: string }>
  exact?: boolean
}

const MAIN_NAV: NavLink[] = [
  { to: '/', label: 'Dashboard', short: 'Home', icon: LayoutDashboard, exact: true },
  { to: '/projects', label: 'Projects', icon: FolderKanban },
  { to: '/cluster', label: 'Cluster', icon: Server },
  { to: '/activity', label: 'Activity', icon: Activity },
]

const SETTINGS_NAV: NavLink[] = [
  { to: '/settings/users', label: 'Users & roles', short: 'Users', icon: Users },
  { to: '/settings/github', label: 'GitHub App', short: 'GitHub', icon: GitBranch },
  { to: '/settings/agents', label: 'AI agents', short: 'Agents', icon: Bot },
  { to: '/settings/audit', label: 'Audit log', short: 'Audit', icon: ScrollText },
  { to: '/settings/s3-targets', label: 'S3 Targets', short: 'S3', icon: Database },
  { to: '/settings/backups', label: 'Backups', icon: Cloud },
  { to: '/settings/updates', label: 'Updates', icon: ArrowUpCircle },
  { to: '/settings/general', label: 'General', icon: Settings },
]

function chromeTitle(pathname: string): { title: string; showShellTitle: boolean } {
  if (pathname === '/') return { title: 'Dashboard', showShellTitle: true }
  if (pathname.startsWith('/projects')) return { title: 'Projects', showShellTitle: pathname === '/projects' }
  if (pathname.startsWith('/cluster')) return { title: 'Cluster', showShellTitle: pathname === '/cluster' || pathname === '/cluster/add' }
  if (pathname.startsWith('/activity')) return { title: 'Activity', showShellTitle: true }
  if (pathname.startsWith('/settings/users')) return { title: 'Users & roles', showShellTitle: true }
  if (pathname.startsWith('/settings/github')) return { title: 'GitHub App', showShellTitle: true }
  if (pathname.startsWith('/settings/agents')) return { title: 'AI agents', showShellTitle: true }
  if (pathname.startsWith('/settings/audit')) return { title: 'Audit log', showShellTitle: true }
  if (pathname.startsWith('/settings/s3-targets')) return { title: 'S3 Targets', showShellTitle: true }
  if (pathname.startsWith('/settings/backups')) return { title: 'Backups', showShellTitle: true }
  if (pathname.startsWith('/settings/updates')) return { title: 'Updates', showShellTitle: true }
  if (pathname.startsWith('/settings/general')) return { title: 'General', showShellTitle: true }
  if (pathname.startsWith('/settings')) return { title: 'Settings', showShellTitle: true }
  return { title: 'AetherixCloud', showShellTitle: false }
}

function navItemClass(collapsed: boolean) {
  return cn(
    'group flex items-center rounded-xl text-[14px] text-muted-foreground transition-[background-color,color] duration-150 hover:bg-muted/70 hover:text-foreground',
    '[&.active]:bg-muted [&.active]:font-semibold [&.active]:text-foreground',
    '[&.active_svg]:text-primary',
    collapsed ? 'justify-center px-0 py-2.5' : 'gap-2.5 px-3 py-2.5',
  )
}

function BurgerButton({ collapsed, onClick }: { collapsed: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
      title={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
      className="flex size-9 shrink-0 flex-col items-center justify-center gap-1 rounded-[10px] border border-border/70 bg-background/50 text-foreground transition-colors hover:bg-muted"
    >
      <span className="block h-[1.5px] w-3.5 rounded-full bg-foreground" />
      <span className="block h-[1.5px] w-3.5 rounded-full bg-foreground" />
      <span className="block h-[1.5px] w-3.5 rounded-full bg-foreground" />
    </button>
  )
}

function SidebarNavLink({ item, collapsed }: { item: NavLink; collapsed: boolean }) {
  const Icon = item.icon
  return (
    <Link
      to={item.to}
      className={navItemClass(collapsed)}
      activeOptions={item.exact ? { exact: true } : undefined}
      title={collapsed ? item.label : undefined}
      aria-label={item.label}
    >
      <Icon className="size-4 shrink-0 opacity-80" />
      {!collapsed && <span className="truncate">{item.label}</span>}
    </Link>
  )
}

function Sidebar({
  clusterName,
  servedBy,
  version,
  live,
  collapsed,
  onToggle,
}: {
  clusterName: string
  servedBy?: string
  version?: string
  live: boolean
  collapsed: boolean
  onToggle: () => void
}) {
  return (
    <aside
      className={cn(
        'ax-sidebar hidden shrink-0 flex-col md:flex',
        collapsed ? 'w-[4.5rem]' : 'w-[13.75rem]',
      )}
    >
      <div
        className={cn(
          'flex shrink-0 items-center',
          collapsed ? 'justify-center px-2 pb-3 pt-3.5' : 'justify-between gap-2 px-3 pb-4 pt-4',
        )}
      >
        {!collapsed && (
          <div className="flex min-w-0 flex-1 items-center gap-2.5">
            <div className="grid size-7 shrink-0 place-items-center rounded-lg bg-primary text-[13px] font-bold text-primary-foreground">
              A
            </div>
            <div className="min-w-0">
              <div className="truncate text-sm font-semibold leading-tight tracking-tight">AetherixCloud</div>
              <div className="truncate text-[11px] text-muted-foreground">{clusterName}</div>
            </div>
          </div>
        )}
        <BurgerButton collapsed={collapsed} onClick={onToggle} />
      </div>

      <nav className={cn('flex-1 space-y-0.5 overflow-y-auto', collapsed ? 'px-1.5' : 'px-2')} aria-label="Main">
        {MAIN_NAV.map((item) => (
          <SidebarNavLink key={item.to} item={item} collapsed={collapsed} />
        ))}
        {collapsed ? (
          <div className="my-2 border-t border-border/60" />
        ) : (
          <div className="px-3 pb-1 pt-4 text-[11px] font-semibold uppercase tracking-[0.04em] text-muted-foreground/70">
            Settings
          </div>
        )}
        {SETTINGS_NAV.map((item) => (
          <SidebarNavLink key={item.to} item={item} collapsed={collapsed} />
        ))}
      </nav>

      {!collapsed && (
        <div className="mx-2 mb-3 mt-2 rounded-2xl border border-border/60 bg-background/45 px-3 py-2.5">
          <div className="text-[11px] text-muted-foreground">Served by</div>
          <div className="truncate text-[13px] font-medium">{servedBy || 'connecting…'}</div>
          <div className="mt-0.5 flex items-center justify-between gap-2">
            {version && <span className="font-mono text-[11px] text-muted-foreground">v{version.replace(/^v/, '')}</span>}
            <span className="inline-flex items-center gap-1.5 text-[11px] text-muted-foreground">
              <span className={cn('size-1.5 rounded-full', live ? 'bg-success' : 'bg-muted-foreground/60')} />
              {live ? 'Live' : 'Polling'}
            </span>
          </div>
        </div>
      )}
    </aside>
  )
}

function MoreSheet({ open, onClose }: { open: boolean; onClose: () => void }) {
  if (!open) return null
  return (
    <div className="fixed inset-0 z-40 md:hidden" role="presentation">
      <button type="button" className="absolute inset-0 bg-black/28 backdrop-blur-[2px]" aria-label="Dismiss" onClick={onClose} />
      <div
        role="dialog"
        aria-label="More settings"
        className="absolute inset-x-0 bottom-0 max-h-[78%] overflow-auto rounded-t-3xl border border-border/70 bg-card px-4 pb-8 pt-3 shadow-[0_-12px_40px_rgb(15_23_32/0.16)]"
      >
        <div className="mb-3 flex items-center justify-between gap-3">
          <div className="text-[17px] font-semibold tracking-tight">More</div>
          <Button variant="secondary" size="sm" onClick={onClose} aria-label="Close">
            <X className="size-3.5" />
            Done
          </Button>
        </div>
        <div className="mb-2 text-[11px] font-semibold uppercase tracking-[0.04em] text-muted-foreground/70">Settings</div>
        <nav className="space-y-0.5" aria-label="Settings">
          {SETTINGS_NAV.map((item) => {
            const Icon = item.icon
            return (
              <Link key={item.to} to={item.to} onClick={onClose} className={navItemClass(false)}>
                <Icon className="size-4 shrink-0 opacity-80" />
                <span>{item.short ?? item.label}</span>
              </Link>
            )
          })}
        </nav>
      </div>
    </div>
  )
}

function MobileBottomTabs({ onMore }: { onMore: () => void }) {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const settingsOpen = pathname.startsWith('/settings')

  return (
    <nav
      className="ax-sidebar fixed inset-x-0 bottom-0 z-30 flex border-t border-border/70 pb-[env(safe-area-inset-bottom)] md:hidden"
      aria-label="Primary"
    >
      {MAIN_NAV.map((item) => {
        const Icon = item.icon
        const active = item.exact ? pathname === item.to : pathname === item.to || pathname.startsWith(`${item.to}/`)
        return (
          <Link
            key={item.to}
            to={item.to}
            className={cn(
              'flex flex-1 flex-col items-center gap-0.5 px-1 py-2 text-[10px] transition-colors',
              active ? 'font-semibold text-primary' : 'text-muted-foreground',
            )}
          >
            <Icon className="size-4" />
            <span>{item.short ?? item.label}</span>
          </Link>
        )
      })}
      <button
        type="button"
        onClick={onMore}
        className={cn(
          'flex flex-1 flex-col items-center gap-0.5 px-1 py-2 text-[10px] transition-colors',
          settingsOpen ? 'font-semibold text-primary' : 'text-muted-foreground',
        )}
      >
        <Menu className="size-4" />
        <span>More</span>
      </button>
    </nav>
  )
}

export function AppLayout() {
  const { user, logout, authDisabled, status: authStatus } = useAuth()
  const { live } = useLiveEvents(authStatus === 'authed')
  const status = useStatus()
  const machines = useMachines()
  const [theme, toggleTheme] = useTheme()
  const [collapsed, toggleCollapsed] = useSidebarCollapsed()
  const [moreOpen, setMoreOpen] = useState(false)
  const nav = useNavigate()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const s = status.data
  const { title, showShellTitle } = chromeTitle(pathname)

  // Status.servedBy is the machine id; prefer the human-readable name in the shell.
  const servedByLabel = (() => {
    const id = s?.servedBy
    if (!id) return undefined
    const match = machines.data?.find((m) => m.id === id)
    return match?.name || id
  })()

  const subtitle = (() => {
    if (!s) return 'connecting…'
    if (pathname.startsWith('/cluster')) {
      return `${s.machinesUp}/${s.machinesTotal} machines up · ${s.replicasRunning}/${s.replicasDesired} replicas`
    }
    const parts = [s.cluster?.name, s.cluster?.network, servedByLabel ? `via ${servedByLabel}` : null].filter(Boolean)
    return parts.join(' · ') || 'Cluster overview'
  })()

  return (
    <div className="flex h-full bg-background">
      <Sidebar
        clusterName={s?.cluster?.name ?? 'cluster'}
        servedBy={servedByLabel}
        version={s?.version}
        live={live}
        collapsed={collapsed}
        onToggle={toggleCollapsed}
      />
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="ax-topbar flex shrink-0 items-center gap-3 border-b border-border/70 px-4 py-3 md:px-5">
          {showShellTitle ? (
            <div className="min-w-0 flex-1">
              <h1 className="truncate text-[1.25rem] font-semibold tracking-[-0.02em] md:text-[1.35rem]">{title}</h1>
              <p className="mt-0.5 hidden truncate text-sm text-muted-foreground sm:block">{subtitle}</p>
            </div>
          ) : (
            <div className="min-w-0 flex-1" />
          )}

          <div className="flex shrink-0 items-center gap-2">
            {pathname === '/' && (
              <>
                <Button variant="secondary" size="sm" className="hidden sm:inline-flex" asChild>
                  <Link to="/cluster/add"><Server className="size-3.5" />Add machine</Link>
                </Button>
                <Button size="sm" className="hidden sm:inline-flex" asChild>
                  <Link to="/projects"><Plus className="size-3.5" />New project</Link>
                </Button>
              </>
            )}
            <button
              type="button"
              className="inline-flex h-9 items-center gap-2 rounded-xl border border-border/70 bg-background/55 px-3 text-sm text-muted-foreground transition-colors hover:bg-muted/70 hover:text-foreground"
              onClick={() => window.dispatchEvent(new Event(openPaletteEvent))}
              aria-label="Open command palette"
            >
              <Search className="size-4 opacity-70" />
              <span className="hidden md:inline">Search</span>
              <kbd className="hidden rounded border bg-muted/80 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground lg:inline">⌘K</kbd>
            </button>
            {isMock && <Badge tone="warning">mock</Badge>}
            <Button variant="ghost" size="icon" onClick={toggleTheme} aria-label="Toggle theme">
              {theme === 'dark' ? <Sun /> : <Moon />}
            </Button>
            {user ? (
              <div className="flex items-center gap-2">
                <div className="hidden text-right text-xs leading-tight lg:block">
                  <div className="font-semibold tracking-tight">{user.name || user.email}</div>
                  <div className="text-muted-foreground">{user.email}</div>
                </div>
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Sign out"
                  onClick={async () => {
                    await logout()
                    void nav({ to: '/login' })
                  }}
                >
                  <LogOut />
                </Button>
              </div>
            ) : (
              authDisabled && <Badge title="AuthService is not available on this node; access is network-trusted">no auth</Badge>
            )}
          </div>
        </header>
        <main className="min-h-0 flex-1 overflow-auto pb-[4.25rem] md:pb-0">
          <Outlet />
        </main>
      </div>
      <MobileBottomTabs onMore={() => setMoreOpen(true)} />
      <MoreSheet open={moreOpen} onClose={() => setMoreOpen(false)} />
      <CommandPalette />
    </div>
  )
}

export function PageHeader({
  title,
  description,
  actions,
  hideTitle,
}: {
  title: string
  description?: string
  actions?: ReactNode
  /** When the shell already shows this page's title. */
  hideTitle?: boolean
}) {
  if (hideTitle && !description && !actions) return null
  return (
    <div className={cn('mb-5 flex flex-wrap items-start justify-between gap-3', hideTitle && !description && 'mb-4')}>
      {!hideTitle || description ? (
        <div className="min-w-0">
          {!hideTitle && <h1 className="text-[1.4rem] font-semibold tracking-tight">{title}</h1>}
          {description && (
            <p className={cn('max-w-2xl text-sm leading-relaxed text-muted-foreground', !hideTitle && 'mt-1')}>{description}</p>
          )}
        </div>
      ) : (
        <div />
      )}
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  )
}

export function SectionLabel({ children }: { children: ReactNode }) {
  return <h2 className="mb-2 text-[11px] font-semibold uppercase tracking-[0.07em] text-muted-foreground">{children}</h2>
}

export function Page({ children, wide }: { children: ReactNode; wide?: boolean }) {
  return <div className={cn('ax-enter mx-auto p-4 md:p-5', wide ? 'max-w-6xl' : 'max-w-4xl')}>{children}</div>
}
