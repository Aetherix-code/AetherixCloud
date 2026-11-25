import { useMemo, useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { Boxes, FolderKanban, Plus, Search } from 'lucide-react'
import { Page, PageHeader } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogHeader } from '@/components/ui/dialog'
import { Empty, ListSkeleton, QueryError } from '@/components/ui/states'
import { Field, Input } from '@/components/ui/input'
import { api } from '@/lib/api'
import { keys, useAction, useProjects, useServices } from '@/lib/queries'
import { timeAgo } from '@/lib/utils'

export function ProjectsPage() {
  const q = useProjects()
  const services = useServices()
  const nav = useNavigate()
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  const [search, setSearch] = useState('')
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

  const counts = useMemo(() => {
    const m = new Map<string, number>()
    for (const s of services.data ?? []) m.set(s.projectId, (m.get(s.projectId) ?? 0) + 1)
    return m
  }, [services.data])

  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase()
    return (q.data ?? []).filter((p) => !needle || p.name.toLowerCase().includes(needle) || p.description.toLowerCase().includes(needle))
  }, [q.data, search])

  return (
    <Page wide>
      <PageHeader
        title="Projects"
        hideTitle
        description="Group services and environments. Open a project to manage the service graph, deploys and variables."
        actions={<Button onClick={() => setOpen(true)}><Plus />New project</Button>}
      />
      {(q.data?.length ?? 0) > 0 && (
        <div className="relative mb-4 max-w-md">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            className="pl-9"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Filter projects…"
            aria-label="Filter projects"
          />
        </div>
      )}
      {q.isLoading ? (
        <ListSkeleton />
      ) : q.error ? (
        <QueryError error={q.error} what="projects" />
      ) : !q.data?.length ? (
        <Card>
          <Empty title="No projects yet" icon={<FolderKanban className="size-8" />}>
            Create a project, then add services from a GitHub repo, a Docker image or a template.
            <div className="mt-4"><Button size="sm" onClick={() => setOpen(true)}><Plus />New project</Button></div>
          </Empty>
        </Card>
      ) : !filtered.length ? (
        <Card><Empty title="No matching projects">Try a different search.</Empty></Card>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {filtered.map((p) => {
            const n = counts.get(p.id) ?? 0
            return (
              <Link key={p.id} to="/projects/$projectId" params={{ projectId: p.id }} search={{}} className="ax-row block">
                <Card className="h-full p-4 transition-[border-color,background-color] duration-150 hover:border-primary/50 hover:bg-card">
                  <div className="flex items-start gap-2">
                    <div className="grid size-8 place-items-center rounded-md bg-primary/10 text-primary">
                      <FolderKanban className="size-4" />
                    </div>
                    <div className="min-w-0 flex-1">
                      <div className="truncate font-semibold tracking-tight">{p.name}</div>
                      <p className="mt-1 line-clamp-2 min-h-8 text-sm text-muted-foreground">{p.description || 'No description'}</p>
                    </div>
                  </div>
                  <div className="mt-4 flex items-center justify-between gap-2 text-xs text-muted-foreground">
                    <Badge tone="neutral" className="gap-1 font-medium">
                      <Boxes className="size-3 opacity-70" />
                      {n} {n === 1 ? 'service' : 'services'}
                    </Badge>
                    <span>Created {timeAgo(p.createdAt)}</span>
                  </div>
                </Card>
              </Link>
            )
          })}
        </div>
      )}
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
