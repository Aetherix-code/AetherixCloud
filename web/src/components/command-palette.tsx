import { useEffect, useState } from 'react'
import { Command } from 'cmdk'
import { useNavigate } from '@tanstack/react-router'
import { Activity, ArrowUpCircle, Bot, Box, Cloud, Container, Database, FolderKanban, GitBranch, LayoutDashboard, Moon, Plus, ScrollText, Server, Settings, Sun, Users } from 'lucide-react'
import { useMachines, useProjects } from '@/lib/queries'
import { useTheme } from '@/lib/theme'

export const openPaletteEvent = 'ax:palette'

/** ⌘K palette: navigation, "New …" actions and jump-to for projects and machines. */
export function CommandPalette() {
  const [open, setOpen] = useState(false)
  const nav = useNavigate()
  const projects = useProjects()
  const machines = useMachines()
  const [theme, toggleTheme] = useTheme()

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen((o) => !o)
      }
    }
    const onOpen = () => setOpen(true)
    window.addEventListener('keydown', onKey)
    window.addEventListener(openPaletteEvent, onOpen)
    return () => {
      window.removeEventListener('keydown', onKey)
      window.removeEventListener(openPaletteEvent, onOpen)
    }
  }, [])

  const go = (fn: () => void) => () => {
    setOpen(false)
    fn()
  }
  const firstProject = projects.data?.[0]
  const newService = (kind: string, extra?: string) =>
    go(() => {
      if (!firstProject) return void nav({ to: '/projects' })
      void nav({ to: '/projects/$projectId', params: { projectId: firstProject.id }, search: { new: kind, template: extra } })
    })

  const item = 'flex cursor-pointer items-center gap-2.5 rounded-md px-2.5 py-2 text-sm aria-selected:bg-primary/10 aria-selected:text-foreground'
  return (
    <Command.Dialog open={open} onOpenChange={setOpen} label="Command palette" className="ax-enter fixed left-1/2 top-[16%] z-[60] w-[calc(100%-2rem)] max-w-xl -translate-x-1/2 overflow-hidden rounded-xl border bg-card/95 ax-panel backdrop-blur-md">
      <Command.Input placeholder="Search or run a command…" className="w-full border-b border-border/80 bg-transparent px-4 py-3.5 text-sm outline-none placeholder:text-muted-foreground" />
      <Command.List className="max-h-80 overflow-y-auto p-2">
        <Command.Empty className="px-3 py-6 text-center text-sm text-muted-foreground">No results.</Command.Empty>
        <Command.Group heading="New" className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5">
          <Command.Item className={item} onSelect={newService('github')}><GitBranch className="size-4" />New → GitHub repo</Command.Item>
          <Command.Item className={item} onSelect={newService('image')}><Container className="size-4" />New → Docker image</Command.Item>
          {['Postgres', 'Redis', 'MySQL', 'MinIO'].map((t) => (
            <Command.Item key={t} className={item} onSelect={newService('template', t.toLowerCase())}><Database className="size-4" />New → Template: {t}</Command.Item>
          ))}
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/cluster/add' }))}><Plus className="size-4" />Add machine</Command.Item>
        </Command.Group>
        <Command.Group heading="Go to" className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5">
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/' }))}><LayoutDashboard className="size-4" />Dashboard</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/projects' }))}><FolderKanban className="size-4" />Projects</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/cluster' }))}><Server className="size-4" />Cluster</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/activity' }))}><Activity className="size-4" />Activity</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/settings/users' }))}><Users className="size-4" />Settings → Users & roles</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/settings/github' }))}><GitBranch className="size-4" />Settings → GitHub App</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/settings/agents' }))}><Bot className="size-4" />Settings → AI agents</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/settings/audit' }))}><ScrollText className="size-4" />Settings → Audit log</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/settings/s3-targets' }))}><Database className="size-4" />Settings → S3 Targets</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/settings/backups' }))}><Cloud className="size-4" />Settings → Backups</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/settings/updates' }))}><ArrowUpCircle className="size-4" />Settings → Updates</Command.Item>
          <Command.Item className={item} onSelect={go(() => void nav({ to: '/settings/general' }))}><Settings className="size-4" />Settings → General</Command.Item>
        </Command.Group>
        {!!projects.data?.length && (
          <Command.Group heading="Projects" className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5">
            {projects.data.map((p) => (
              <Command.Item key={p.id} value={`project ${p.name}`} className={item} onSelect={go(() => void nav({ to: '/projects/$projectId', params: { projectId: p.id }, search: {} }))}><Box className="size-4" />{p.name}</Command.Item>
            ))}
          </Command.Group>
        )}
        {!!machines.data?.length && (
          <Command.Group heading="Machines" className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5">
            {machines.data.map((m) => (
              <Command.Item key={m.id} value={`machine ${m.name}`} className={item} onSelect={go(() => void nav({ to: '/cluster/machines/$machineId', params: { machineId: m.id } }))}><Server className="size-4" />{m.name}</Command.Item>
            ))}
          </Command.Group>
        )}
        <Command.Group heading="Preferences" className="text-xs text-muted-foreground [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1.5">
          <Command.Item className={item} onSelect={go(toggleTheme)}>{theme === 'dark' ? <Sun className="size-4" /> : <Moon className="size-4" />}Switch to {theme === 'dark' ? 'light' : 'dark'} theme</Command.Item>
        </Command.Group>
      </Command.List>
    </Command.Dialog>
  )
}
