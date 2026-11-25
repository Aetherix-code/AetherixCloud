import { Link, Outlet, useNavigate } from '@tanstack/react-router'
import { Activity, ArrowUpCircle, Bot, Cloud, FolderKanban, GitBranch, LayoutDashboard, LogOut, Moon, ScrollText, Search, Server, Settings, Sun, Users } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { CommandPalette, openPaletteEvent } from '@/components/command-palette'
import { useAuth } from '@/lib/auth'
import { useLiveEvents } from '@/lib/live'
import { isMock } from '@/lib/api'
import { useStatus } from '@/lib/queries'
import { useTheme } from '@/lib/theme'
import { cn } from '@/lib/utils'

const navItem =
  'group flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-[13px] text-muted-foreground transition-[background-color,color,transform] duration-150 hover:bg-muted/80 hover:text-foreground [&.active]:bg-primary/12 [&.active]:font-semibold [&.active]:text-foreground'

export function AppLayout() {
  const { user, logout, authDisabled, status: authStatus } = useAuth()
  const { live } = useLiveEvents(authStatus === 'authed')
  const status = useStatus()
  const [theme, toggleTheme] = useTheme()
  const nav = useNavigate()
  const s = status.data
  return (
    <div className="flex h-full">
      <aside className="ax-shell hidden w-[15.75rem] shrink-0 flex-col border-r md:flex">
        <div className="flex h-14 items-center gap-2.5 border-b border-border/70 px-3.5">
          <div className="grid size-8 place-items-center rounded-lg bg-primary text-sm font-bold text-primary-foreground shadow-sm">A</div>
          <div className="min-w-0">
            <div className="truncate text-sm font-semibold tracking-tight leading-tight">{s?.cluster?.name ?? 'AetherixCloud'}</div>
            <div className="truncate text-[11px] text-muted-foreground">{s?.servedBy ? `via ${s.servedBy}` : 'connecting…'}</div>
          </div>
        </div>
        <nav className="flex-1 space-y-0.5 overflow-y-auto p-2.5" aria-label="Main">
          <Link to="/" className={navItem} activeOptions={{ exact: true }}><LayoutDashboard className="size-4 opacity-80" />Dashboard</Link>
          <Link to="/projects" className={navItem}><FolderKanban className="size-4 opacity-80" />Projects</Link>
          <Link to="/cluster" className={navItem}><Server className="size-4 opacity-80" />Cluster</Link>
          <Link to="/activity" className={navItem}><Activity className="size-4 opacity-80" />Activity</Link>
          <div className="px-2.5 pb-1 pt-5 text-[10px] font-semibold uppercase tracking-[0.08em] text-muted-foreground/80">Settings</div>
          <Link to="/settings/users" className={navItem}><Users className="size-4 opacity-80" />Users & roles</Link>
          <Link to="/settings/github" className={navItem}><GitBranch className="size-4 opacity-80" />GitHub App</Link>
          <Link to="/settings/agents" className={navItem}><Bot className="size-4 opacity-80" />AI agents</Link>
          <Link to="/settings/audit" className={navItem}><ScrollText className="size-4 opacity-80" />Audit log</Link>
          <Link to="/settings/backups" className={navItem}><Cloud className="size-4 opacity-80" />Backups</Link>
          <Link to="/settings/updates" className={navItem}><ArrowUpCircle className="size-4 opacity-80" />Updates</Link>
          <Link to="/settings/general" className={navItem}><Settings className="size-4 opacity-80" />General</Link>
        </nav>
        <div className="border-t border-border/70 p-3 text-xs text-muted-foreground">
          {s && (
            <div className="mb-2 grid grid-cols-2 gap-x-2 gap-y-0.5 tabular-nums">
              <span>{s.machinesUp}/{s.machinesTotal} up</span>
              <span className="text-right">{s.replicasRunning}/{s.replicasDesired} reps</span>
              {s.version && <span className="col-span-2 font-mono text-[10px] opacity-80">v{s.version}</span>}
            </div>
          )}
          <div className="flex items-center gap-1.5">
            <span className={cn('size-1.5 rounded-full', live ? 'bg-success' : 'bg-muted-foreground/60')} />
            {live ? 'Live' : 'Polling'}
          </div>
        </div>
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="ax-shell flex h-14 shrink-0 items-center gap-3 border-b px-4">
          <button
            className="flex h-9 w-full max-w-md cursor-pointer items-center gap-2 rounded-md border bg-background/70 px-3 text-sm text-muted-foreground transition-colors hover:border-primary/30 hover:bg-muted/60 hover:text-foreground"
            onClick={() => window.dispatchEvent(new Event(openPaletteEvent))}
            aria-label="Open command palette"
          >
            <Search className="size-4 opacity-70" />
            <span className="flex-1 text-left">Search or jump to…</span>
            <kbd className="rounded border bg-muted/80 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">⌘K</kbd>
          </button>
          <div className="ml-auto flex items-center gap-2">
            {isMock && <Badge tone="warning">mock data</Badge>}
            <Button variant="ghost" size="icon" onClick={toggleTheme} aria-label="Toggle theme">{theme === 'dark' ? <Sun /> : <Moon />}</Button>
            {user ? (
              <div className="flex items-center gap-2">
                <div className="hidden text-right text-xs leading-tight sm:block">
                  <div className="font-semibold tracking-tight">{user.name || user.email}</div>
                  <div className="text-muted-foreground">{user.email}</div>
                </div>
                <Button variant="ghost" size="icon" aria-label="Sign out" onClick={async () => { await logout(); void nav({ to: '/login' }) }}><LogOut /></Button>
              </div>
            ) : (
              authDisabled && <Badge title="AuthService is not available on this node; access is network-trusted">no auth</Badge>
            )}
          </div>
        </header>
        <main className="min-h-0 flex-1 overflow-auto">
          <Outlet />
        </main>
      </div>
      <CommandPalette />
    </div>
  )
}

export function PageHeader({ title, description, actions }: { title: string; description?: string; actions?: React.ReactNode }) {
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

export function SectionLabel({ children }: { children: React.ReactNode }) {
  return <h2 className="mb-2 text-[11px] font-semibold uppercase tracking-[0.07em] text-muted-foreground">{children}</h2>
}

export function Page({ children, wide }: { children: React.ReactNode; wide?: boolean }) {
  return <div className={cn('ax-enter mx-auto p-5 md:p-6', wide ? 'max-w-6xl' : 'max-w-4xl')}>{children}</div>
}
