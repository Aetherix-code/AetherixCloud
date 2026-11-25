import { useCallback, useEffect, useMemo, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Background, BackgroundVariant, Controls, Handle, MarkerType, Position, ReactFlow, useEdgesState, useNodesState, type Connection, type Edge, type Node, type NodeProps } from '@xyflow/react'
import { Box, Container, GitBranch, Globe } from 'lucide-react'
import type { Domain, Replica, Service, Variable } from '@/gen/aetherix/v1/types_pb'
import { ReplicaState } from '@/gen/aetherix/v1/types_pb'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogHeader } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { toast } from '@/components/ui/toast'
import { api, errMessage } from '@/lib/api'
import { keys } from '@/lib/queries'
import { Dot } from '@/lib/status'
import type { Tone } from '@/components/ui/badge'
import { layoutServices } from '@/lib/layout'
import type { RefEdge } from '@/lib/refs'
import { cn } from '@/lib/utils'

export interface ServiceNodeData extends Record<string, unknown> {
  service: Service
  replicas: Replica[]
  domain?: Domain
  selected: boolean
}
type ServiceNode = Node<ServiceNodeData, 'service'>

export function serviceHealth(s: Service, replicas: Replica[]): { tone: Tone; label: string } {
  const desired = s.spec?.replicas ?? 0
  const running = replicas.filter((r) => r.state === ReplicaState.RUNNING).length
  if (!s.activeDeploymentId && !replicas.length) return { tone: 'neutral', label: 'not deployed' }
  if (replicas.some((r) => r.state === ReplicaState.STRANDED)) return { tone: 'warning', label: 'stranded' }
  if (replicas.some((r) => r.state === ReplicaState.FAILED)) return { tone: 'danger', label: 'failing' }
  if (running >= desired && desired > 0) return { tone: 'success', label: `${running}/${desired} running` }
  if (replicas.some((r) => r.state === ReplicaState.STARTING || r.state === ReplicaState.PENDING)) return { tone: 'info', label: 'starting' }
  return { tone: 'warning', label: `${running}/${desired} running` }
}

function ServiceNodeView({ data }: NodeProps<ServiceNode>) {
  const { service: s, replicas, domain } = data
  const h = serviceHealth(s, replicas)
  const gh = s.spec?.source?.github
  return (
    <div className={cn('w-64 cursor-pointer rounded-lg border bg-card p-3 shadow-sm transition-shadow hover:shadow-md', data.selected && 'border-primary ring-2 ring-primary/40')}>
      <Handle type="target" position={Position.Left} className="!size-2.5 !border-2 !border-background !bg-muted-foreground" title="Referenced by other services" />
      <div className="flex items-center gap-2">
        <div className="grid size-7 place-items-center rounded-md bg-muted">{gh ? <GitBranch className="size-4" /> : s.spec?.source?.image ? <Container className="size-4" /> : <Box className="size-4" />}</div>
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold">{s.name}</div>
          <div className="truncate text-[11px] text-muted-foreground">{gh ? `${gh.repo}@${gh.branch}` : s.spec?.source?.image || s.spec?.image}</div>
        </div>
      </div>
      <div className="mt-3 flex items-center gap-1.5 text-xs">
        <Dot tone={h.tone} pulse={h.tone === 'info'} />
        <span className="text-muted-foreground">{h.label}</span>
      </div>
      {domain && (
        <div className="mt-1.5 flex items-center gap-1.5 truncate text-[11px] text-muted-foreground"><Globe className="size-3" />{domain.hostname}</div>
      )}
      <Handle type="source" position={Position.Right} className="!size-2.5 !border-2 !border-background !bg-primary" title="Drag to another service to create ${{service.KEY}}" />
    </div>
  )
}

const nodeTypes = { service: ServiceNodeView }
const posKey = (projectId: string) => `ax.canvas.${projectId}`

export function ProjectCanvas({ projectId, environmentId, services, replicas, domains, edges, variables, selected, onSelect }: {
  projectId: string
  environmentId: string
  services: Service[]
  replicas: Replica[]
  domains: Domain[]
  edges: RefEdge[]
  variables: Variable[]
  selected?: string
  onSelect: (id?: string) => void
}) {
  const qc = useQueryClient()
  const initial = useCallback(
    (s: Service, auto: Record<string, { x: number; y: number }>) => {
      let saved: Record<string, { x: number; y: number }> = {}
      try {
        saved = JSON.parse(localStorage.getItem(posKey(projectId)) ?? '{}')
      } catch {
        /* ignore corrupt layout */
      }
      return saved[s.id] ?? auto[s.name]
    },
    [projectId],
  )

  const [nodes, setNodes, onNodesChange] = useNodesState<ServiceNode>([])
  const [rfEdges, setEdges] = useEdgesState<Edge>([])
  const [link, setLink] = useState<{ fromId: string; toId: string } | null>(null)
  const [varName, setVarName] = useState('DATABASE_URL')
  const [refKey, setRefKey] = useState('')
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    const auto = layoutServices(services.map((s) => s.name), edges)
    setNodes(() =>
      services.map((s) => ({
        id: s.id,
        type: 'service' as const,
        position: initial(s, auto),
        data: { service: s, replicas: replicas.filter((r) => r.serviceId === s.id), domain: domains.find((d) => d.serviceId === s.id && !d.generated) ?? domains.find((d) => d.serviceId === s.id), selected: s.id === selected },
      })),
    )
  }, [services, replicas, domains, selected, edges, initial, setNodes])

  useEffect(() => {
    const byName = new Map(services.map((s) => [s.name, s.id]))
    setEdges(
      edges.map((e) => ({
        id: `${e.from}->${e.to}`,
        source: byName.get(e.from)!,
        target: byName.get(e.to)!,
        label: e.key,
        animated: true,
        markerEnd: { type: MarkerType.ArrowClosed },
        labelStyle: { fontSize: 10, fill: 'var(--muted-foreground)' },
        labelBgStyle: { fill: 'var(--card)' },
      })),
    )
  }, [edges, services, setEdges])

  const persist = useCallback(
    (_: unknown, node: Node) => {
      try {
        const cur = JSON.parse(localStorage.getItem(posKey(projectId)) ?? '{}')
        cur[node.id] = node.position
        localStorage.setItem(posKey(projectId), JSON.stringify(cur))
      } catch {
        /* storage unavailable */
      }
    },
    [projectId],
  )

  const onConnect = useCallback((conn: Connection) => {
    if (!conn.source || !conn.target || conn.source === conn.target) return
    const from = services.find((s) => s.id === conn.source)
    const to = services.find((s) => s.id === conn.target)
    if (!from || !to) return
    const keysOnTarget = variables.filter((v) => v.serviceId === to.id).map((v) => v.key)
    setVarName(keysOnTarget[0] || 'DATABASE_URL')
    setRefKey(keysOnTarget[0] || '')
    setLink({ fromId: from.id, toId: to.id })
  }, [services, variables])

  const fromSvc = useMemo(() => services.find((s) => s.id === link?.fromId), [services, link])
  const toSvc = useMemo(() => services.find((s) => s.id === link?.toId), [services, link])
  const targetKeys = useMemo(() => variables.filter((v) => v.serviceId === link?.toId), [variables, link])

  const confirmLink = async () => {
    if (!link || !fromSvc || !toSvc || !varName || !refKey) return
    setSaving(true)
    try {
      const token = `\${{${toSvc.name}.${refKey}}}`
      await api.projects.setVariables({
        environmentId,
        serviceId: fromSvc.id,
        redeploy: false,
        variables: [{ key: varName.toUpperCase(), value: token, secret: false }],
      })
      toast.success(`Linked ${fromSvc.name}.${varName.toUpperCase()} → ${toSvc.name}.${refKey}`)
      void qc.invalidateQueries({ queryKey: keys.variables(environmentId, '*') })
      void qc.invalidateQueries({ queryKey: keys.variables(environmentId, fromSvc.id) })
      setLink(null)
    } catch (e) {
      toast.error(errMessage(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="h-full w-full" data-testid="project-canvas">
      <ReactFlow
        nodes={nodes}
        edges={rfEdges}
        nodeTypes={nodeTypes}
        onNodesChange={onNodesChange}
        onConnect={onConnect}
        onNodeClick={(_, n) => onSelect(n.id)}
        onPaneClick={() => onSelect(undefined)}
        onNodeDragStop={persist}
        fitView
        fitViewOptions={{ padding: 0.3, maxZoom: 1 }}
        minZoom={0.3}
        proOptions={{ hideAttribution: true }}
        connectionLineStyle={{ stroke: 'var(--primary)', strokeWidth: 2 }}
      >
        <Background variant={BackgroundVariant.Dots} gap={20} size={1} />
        <Controls showInteractive={false} />
      </ReactFlow>
      <p className="pointer-events-none absolute bottom-3 left-3 max-w-sm text-[11px] text-muted-foreground">
        Drag from the right handle of a service to another to create a <code className="rounded bg-muted px-1">{'${{service.KEY}}'}</code> variable reference. Existing references draw the edges.
      </p>
      <Dialog open={!!link} onOpenChange={(o) => !o && setLink(null)}>
        <DialogContent>
          <DialogHeader
            title="Create variable reference"
            description={fromSvc && toSvc ? `${fromSvc.name} will reference a variable from ${toSvc.name}.` : undefined}
          />
          <div className="space-y-3">
            <Field label={`Variable on ${fromSvc?.name ?? 'service'}`}>
              <Input value={varName} onChange={(e) => setVarName(e.target.value.toUpperCase())} placeholder="DATABASE_URL" className="font-mono text-xs" />
            </Field>
            <Field label={`Key on ${toSvc?.name ?? 'target'}`}>
              {targetKeys.length ? (
                <Select value={refKey} onChange={(e) => setRefKey(e.target.value)}>
                  <option value="">Select key…</option>
                  {targetKeys.map((v) => <option key={v.id} value={v.key}>{v.key}</option>)}
                </Select>
              ) : (
                <Input value={refKey} onChange={(e) => setRefKey(e.target.value)} placeholder="DATABASE_URL" className="font-mono text-xs" />
              )}
            </Field>
            {fromSvc && toSvc && refKey && (
              <p className="rounded-md border bg-muted/50 p-2 font-mono text-xs">{`${varName || 'KEY'}=\${{${toSvc.name}.${refKey}}}`}</p>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" onClick={() => setLink(null)}>Cancel</Button>
              <Button onClick={() => void confirmLink()} disabled={!varName || !refKey || saving}>{saving ? 'Saving…' : 'Create link'}</Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
