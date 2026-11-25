import { Link, Outlet, useNavigate, useRouterState } from '@tanstack/react-router'
import {
  Activity,
  ArrowUpCircle,
  Bot,
  Cloud,
  FolderKanban,
  GitBranch,
  LayoutDashboard,
  LogOut,
  Menu,
  Moon,
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
import { useStatus } from '@/lib/queries'
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
  { to: '/settings/backups', label: 'Backups', icon: Cloud },
  { to: '/settings/updates', label: 'Updates', icon: ArrowUpCircle },
  { to: '/settings/general', label: 'General', icon: Settings },
]

function navItemClass(collapsed: boolean) {
  return cn(
    'group flex items-center rounded-xl text-[13px] text-muted-foreground transition-[background-color,color,transform] duration-150 hover:bg-muted/80 hover:text-foreground',
    '[&.active]:bg-primary/12 [&.active]:font-semibold [&.active]:text-foreground',
    collapsed ? 'justify-center px-0 py-2.5' : 'gap-2.5 px-2.5 py-2',
  )
}

function BurgerButton({ collapsed, onClick }: { collapsed: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
      title={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
      className="grid size-9 shrink-0 place-items-center rounded-[10px] border border-border/80 bg-muted/50 text-foreground transition-colors hover:bg-muted"
    >
      <Menu className="size-4" />
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
  machinesUp,
  machinesTotal,
  replicasRunning,
  replicasDesired,
  live,
  collapsed,
  onToggle,
}: {
  clusterName: string
  servedBy?: string
  version?: string
  machinesUp?: number
  machinesTotal?: number
  replicasRunning?: number
  replicasDesired?: number
  live: boolean
  collapsed: boolean
  onToggle: () => void
}) {
  return (
    <aside
      className={cn(
        'ax-shell hidden shrink-0 flex-col border-r md:flex',
        collapsed ? 'w-[4.5rem]' : 'w-[15.75rem]',
      )}
    >
      <div
        className={cn(
          'flex h-14 shrink-0 items-center border-b border-border/70',
          collapsed ? 'justify-center px-2' : 'gap-2.5 px-3',
        )}
      >
        {!collapsed && (
          <div className="flex min-w-0 flex-1 items-center gap-2.5">
            <div className="grid size-8 shrink-0 place-items-center rounded-lg bg-primary text-sm font-bold text-primary-foreground shadow-sm">
              A
            </div>
            <div className="min-w-0">
              <div className="truncate text-sm font-semibold leading-tight tracking-tight">{clusterName}</div>
              <div className="truncate text-[11px] text-muted-foreground">
                {servedBy ? `via ${servedBy}` : 'connecting…'}
              </div>
            </div>
          </div>
        )}
        <BurgerButton collapsed={collapsed} onClick={onToggle} />
      </div>

      <nav className={cn('flex-1 space-y-0.5 overflow-y-auto', collapsed ? 'p-1.5' : 'p-2.5')} aria-label="Main">
        {MAIN_NAV.map((item) => (
          <SidebarNavLink key={item.to} item={item} collapsed={collapsed} />
        ))}
        {collapsed ? (
          <div className="my-2 border-t border-border/70" />
        ) : (
          <div className="px-2.5 pb-1 pt-5 text-[10px] font-semibold uppercase tracking-[0.08em] text-muted-foreground/80">
            Settings
          </div>
        )}
        {SETTINGS_NAV.map((item) => (
          <SidebarNavLink key={item.to} item={item} collapsed={collapsed} />
        ))}
      </nav>

      {!collapsed && (
        <div className="border-t border-border/70 p-3 text-xs text-muted-foreground">
          {(machinesTotal != null || replicasDesired != null) && (
            <div className="mb-2 grid grid-cols-2 gap-x-2 gap-y-0.5 tabular-nums">
              {machinesTotal != null && (
                <span>
                  {machinesUp}/{machinesTotal} up
                </span>
              )}
              {replicasDesired != null && (
                <span className="text-right">
                  {replicasRunning}/{replicasDesired} reps
                </span>
              )}
              {version && <span className="col-span-2 font-mono text-[10px] opacity-80">v{version}</span>}
            </div>
          )}
          <div className="flex items-center gap-1.5">
            <span className={cn('size-1.5 rounded-full', live ? 'bg-success' : 'bg-muted-foreground/60')} />
            {live ? 'Live' : 'Polling'}
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
      <button type="button" className="absolute inset-0 bg-black/40 backdrop-blur-[2px]" aria-label="Dismiss" onClick={onClose} />
      <div
        role="dialog"
        aria-label="More settings"
        className="absolute inset-x-0 bottom-0 max-h-[78%] overflow-auto rounded-t-[1.5rem] border border-border/80 bg-card px-4 pb-8 pt-3 shadow-[0_-12px_40px_rgb(15_23_32/0.18)]"
      >
        <div className="mb-3 flex items-center justify-between gap-3">
          <div className="text-[17px] font-semibold tracking-tight">More</div>
          <Button variant="secondary" size="sm" onClick={onClose} aria-label="Close">
            <X className="size-3.5" />
            Done
          </Button>
        </div>
        <div className="mb-2 text-[10px] font-semibold uppercase tracking-[0.08em] text-muted-foreground/80">Settings</div>
        <nav className="space-y-0.5" aria-label="Settings">
          {SETTINGS_NAV.map((item) => {
            const Icon = item.icon
            return (
              <Link
                key={item.to}
                to={item.to}
                onClick={onClose}
                className={navItemClass(false)}
              >
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
      className="ax-shell fixed inset-x-0 bottom-0 z-30 flex border-t pb-[env(safe-area-inset-bottom)] md:hidden"
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
  const [theme, toggleTheme] = useTheme()
  const [collapsed, toggleCollapsed] = useSidebarCollapsed()
  const [moreOpen, setMoreOpen] = useState(false)
  const nav = useNavigate()
  const s = status.data

  return (
    <div className="flex h-full">
      <Sidebar
        clusterName={s?.cluster?.name ?? 'AetherixCloud'}
        servedBy={s?.servedBy}
        version={s?.version}
        machinesUp={s?.machinesUp}
        machinesTotal={s?.machinesTotal}
        replicasRunning={s?.replicasRunning}
        replicasDesired={s?.replicasDesired}
        live={live}
        collapsed={collapsed}
        onToggle={toggleCollapsed}
      />
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="ax-shell flex h-14 shrink-0 items-center gap-3 border-b px-4">
          <button
            className="flex h-9 w-full max-w-md cursor-pointer items-center gap-2 rounded-xl border bg-background/70 px-3 text-sm text-muted-foreground transition-colors hover:border-primary/30 hover:bg-muted/60 hover:text-foreground"
            onClick={() => window.dispatchEvent(new Event(openPaletteEvent))}
            aria-label="Open command palette"
          >
            <Search className="size-4 opacity-70" />
            <span className="flex-1 text-left">Search or jump to…</span>
            <kbd className="rounded border bg-muted/80 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">⌘K</kbd>
          </button>
          <div className="ml-auto flex items-center gap-2">
            {isMock && <Badge tone="warning">mock data</Badge>}
            <Button variant="ghost" size="icon" onClick={toggleTheme} aria-label="Toggle theme">
              {theme === 'dark' ? <Sun /> : <Moon />}
            </Button>
            {user ? (
              <div className="flex items-center gap-2">
                <div className="hidden text-right text-xs leading-tight sm:block">
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

export function PageHeader({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return (
    <div className="mb-5 flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0">
        <h1 className="text-[1.4rem] font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-1 max-w-2xl text-sm leading-relaxed text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  )
}

export function SectionLabel({ children }: { children: ReactNode }) {
  return <h2 className="mb-2 text-[11px] font-semibold uppercase tracking-[0.07em] text-muted-foreground">{children}</h2>
}

export function Page({ children, wide }: { children: ReactNode; wide?: boolean }) {
  return <div className={cn('ax-enter mx-auto p-5 md:p-6', wide ? 'max-w-6xl' : 'max-w-4xl')}>{children}</div>
}
