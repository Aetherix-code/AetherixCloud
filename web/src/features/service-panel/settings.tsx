import { useState } from 'react'
import { AlertTriangle, Globe, HardDrive, Hammer, Pencil, Plus, RotateCw, Trash2 } from 'lucide-react'
import { useNavigate } from '@tanstack/react-router'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Field, Input, Select } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { VolumeFields } from '@/features/volume-form'
import { DeployStrategy, PortProtocol, ReplicaState, type Domain, type Replica, type Service, type ServicePort, type ServiceSpec, type VolumeMount } from '@/gen/aetherix/v1/types_pb'
import { api } from '@/lib/api'
import { keys, useAction, useMachines } from '@/lib/queries'
import { detectPlan } from '@/lib/railpack'
import { emptyVolume, volumeFromSpec, volumeToSpec, volumeValid, type VolumeDraft } from '@/lib/volumes'
import { formatBytes, tsDate } from '@/lib/utils'

function Section({ title, description, children, icon }: { title: string; description?: string; children: React.ReactNode; icon?: React.ReactNode }) {
  return (
    <Card>
      <CardHeader><CardTitle className="flex items-center gap-2">{icon}{title}</CardTitle>{description && <CardDescription>{description}</CardDescription>}</CardHeader>
      <CardContent className="space-y-4">{children}</CardContent>
    </Card>
  )
}

/** Partial spec update using proto field names in the update mask. */
function useSpecUpdate(service: Service) {
  return useAction(
    (a: { spec: Partial<ServiceSpec> | Record<string, unknown>; mask: string[]; redeploy?: boolean }) =>
      api.services.updateService({ serviceId: service.id, spec: a.spec as never, updateMask: a.mask }).then(async (r) => {
        if (a.redeploy) await api.deploy.deploy({ serviceId: service.id, redeploy: true })
        return r
      }),
    { invalidate: [keys.service(service.id), ['project'], keys.deployments(service.id)], success: 'Settings saved' },
  )
}

export function SettingsTab({ service, replicas, domains }: { service: Service; replicas: Replica[]; domains: Domain[] }) {
  const spec = service.spec
  return (
    <div className="space-y-4">
      <SourceSection service={service} />
      <BuildSection service={service} />
      <PortsSection service={service} />
      <ScalingSection service={service} replicas={replicas} />
      <ReschedulableSection service={service} replicas={replicas} />
      <DomainsSection service={service} domains={domains} />
      <VolumesSection service={service} />
      <ResourcesSection service={service} />
      <HealthSection service={service} />
      {spec && <DangerSection service={service} />}
    </div>
  )
}

function SourceSection({ service }: { service: Service }) {
  const src = service.spec?.source
  const currentImage = src?.image || service.spec?.image || ''
  const [image, setImage] = useState(currentImage)
  const [branch, setBranch] = useState(src?.github?.branch ?? 'main')
  const save = useSpecUpdate(service)

  if (src?.github) {
    return (
      <Section title="Source" description="GitHub source for this service. Image is produced by the build pipeline.">
        <div className="text-sm">
          GitHub <code className="rounded bg-muted px-1">{src.github.repo}</code>
          <div className="mt-1 text-xs text-muted-foreground">Pushes build and deploy automatically{src.github.prPreviews ? '; PR previews are enabled' : ''}.</div>
        </div>
        <Field label="Branch">
          <Input value={branch} onChange={(e) => setBranch(e.target.value)} />
        </Field>
        {service.spec?.image && (
          <p className="text-xs text-muted-foreground">Running image <code className="rounded bg-muted px-1">{service.spec.image}</code></p>
        )}
        <div className="flex justify-end">
          <Button
            size="sm"
            variant="secondary"
            disabled={save.isPending || branch === (src.github.branch || 'main')}
            onClick={() => save.mutate({
              mask: ['source'],
              redeploy: true,
              spec: { source: { ...src, github: { ...src.github!, branch } } },
            })}
          >
            Save branch & redeploy
          </Button>
        </div>
      </Section>
    )
  }

  return (
    <Section title="Source" description="Change the Docker image and redeploy. Digests are pinned after the next deploy.">
      <Field label="Docker image" hint="registry/name:tag — for example postgres:16 or ghcr.io/org/app:main">
        <Input value={image} onChange={(e) => setImage(e.target.value)} placeholder="ghcr.io/org/app:tag" className="font-mono text-xs" />
      </Field>
      <div className="flex justify-end">
        <Button
          size="sm"
          disabled={save.isPending || !image.trim() || image.trim() === currentImage}
          onClick={() => save.mutate({
            mask: ['source', 'image'],
            redeploy: true,
            spec: { source: { image: image.trim() }, image: image.trim() },
          })}
        >
          Save image & redeploy
        </Button>
      </div>
    </Section>
  )
}

function BuildSection({ service }: { service: Service }) {
  const b = service.spec?.build
  const [f, setF] = useState({
    builder: b?.builder ?? '',
    context: b?.context ?? '',
    dockerfile: b?.dockerfile ?? '',
    buildCommand: b?.buildCommand ?? '',
    startCommand: b?.startCommand ?? '',
    platform: b?.platform ?? '',
    watch: (b?.watchPaths ?? []).join(', '),
  })
  const save = useSpecUpdate(service)
  const plan = detectPlan(service)
  if (!service.spec?.source?.github) return null
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => {
    const next = { ...f, [k]: e.target.value }
    // When root directory is set and watch paths are empty, default to root/**
    if (k === 'context' && !f.watch.trim() && e.target.value.trim() && e.target.value.trim() !== '/') {
      const root = e.target.value.trim().replace(/^\/+|\/+$/g, '')
      next.watch = `${root}/**`
    }
    setF(next)
  }
  return (
    <Section title="Build & monorepo" icon={<Hammer className="size-4" />} description="Point this service at a subdirectory of the repo. Only matching pushes rebuild it.">
      <div className="rounded-md border bg-muted/40 p-3 text-sm" aria-label="Detected build plan">
        <div className="mb-2 flex items-center gap-2 font-medium">Detected plan <Badge tone="info">{plan.provider}</Badge>{plan.source === 'heuristic' && <Badge>preview</Badge>}</div>
        {plan.provider === 'railpack' ? (
          <dl className="grid grid-cols-[6rem_1fr] gap-y-1 text-xs">
            <dt className="text-muted-foreground">Language</dt><dd>{plan.language} {plan.version}</dd>
            <dt className="text-muted-foreground">Install</dt><dd><code>{plan.install || '—'}</code></dd>
            <dt className="text-muted-foreground">Build</dt><dd><code>{plan.build || '—'}</code></dd>
            <dt className="text-muted-foreground">Start</dt><dd><code>{plan.start || '—'}</code></dd>
          </dl>
        ) : (
          <p className="text-xs text-muted-foreground">Building with {plan.build ?? 'the configured Dockerfile'}.</p>
        )}
        {plan.source === 'heuristic' && <p className="mt-2 text-xs text-muted-foreground">Estimated from the repository name; the real Railpack plan is shown after the first build. Override any command below.</p>}
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Builder"><Select value={f.builder} onChange={set('builder')}><option value="">Auto-detect</option><option value="dockerfile">Dockerfile</option><option value="railpack">Railpack</option></Select></Field>
        <Field label="Root directory" hint="Monorepo path relative to the repo root, e.g. apps/api"><Input value={f.context} onChange={set('context')} placeholder="apps/web" className="font-mono text-xs" /></Field>
        <Field label="Dockerfile path" hint="Relative to the root directory"><Input value={f.dockerfile} onChange={set('dockerfile')} placeholder="Dockerfile" className="font-mono text-xs" /></Field>
        <Field label="Platform"><Input value={f.platform} onChange={set('platform')} placeholder="linux/amd64" /></Field>
        <Field label="Build command"><Input value={f.buildCommand} onChange={set('buildCommand')} className="font-mono text-xs" /></Field>
        <Field label="Start command"><Input value={f.startCommand} onChange={set('startCommand')} className="font-mono text-xs" /></Field>
      </div>
      <Field
        label="Watch paths"
        hint="Comma-separated globs. Empty with a root directory defaults to <root>/** so sibling packages do not trigger rebuilds. Include shared packages explicitly, e.g. apps/web/**, packages/ui/**."
      >
        <Input value={f.watch} onChange={set('watch')} placeholder="apps/web/**, packages/ui/**" className="font-mono text-xs" />
      </Field>
      <div className="flex justify-end gap-2">
        <Button size="sm" disabled={save.isPending} onClick={() => save.mutate({ mask: ['build'], redeploy: true, spec: { build: { ...b, builder: f.builder, context: f.context.replace(/^\/+|\/+$/g, ''), dockerfile: f.dockerfile, buildCommand: f.buildCommand, startCommand: f.startCommand, platform: f.platform, watchPaths: f.watch.split(',').map((x) => x.trim()).filter(Boolean), imageRetain: b?.imageRetain ?? 0, imageS3TargetId: b?.imageS3TargetId ?? '' } } })}>Save & rebuild</Button>
      </div>
    </Section>
  )
}

function PortsSection({ service }: { service: Service }) {
  const ports = service.spec?.ports ?? []
  const save = useSpecUpdate(service)
  const [mode, setMode] = useState('internal')
  const [containerPort, setContainerPort] = useState(ports[0]?.containerPort || 8080)

  const add = () => {
    const row = {
      containerPort,
      protocol: PortProtocol.TCP,
      mode,
      // tcp: leave 0 so the API assigns one random cluster-wide ingress port
      publishedPort: 0,
    } as ServicePort
    save.mutate({ mask: ['ports'], redeploy: true, spec: { ports: [...ports, row] } })
  }
  const remove = (i: number) => {
    save.mutate({ mask: ['ports'], redeploy: true, spec: { ports: ports.filter((_, j) => j !== i) } })
  }

  return (
    <Section
      title="Ports & networking"
      description="Declare container ports for mesh DNS (internal) or a shared TCP proxy on every ingress node. Public web URLs are managed under Domains."
    >
      <div className="divide-y rounded-md border">
        {ports.length === 0 && <p className="p-3 text-sm text-muted-foreground">No ports declared. Services still reach each other as name.internal on the mesh.</p>}
        {ports.map((p, i) => (
          <div key={`${p.mode}-${p.containerPort}-${i}`} className="flex items-center gap-2 p-2.5 text-sm">
            <Badge tone={p.mode === 'tcp' ? 'info' : undefined}>{p.mode || 'internal'}</Badge>
            {p.mode === 'tcp' && !!p.publishedPort ? (
              <span className="font-mono text-xs">ingress :{p.publishedPort} → container :{p.containerPort}</span>
            ) : (
              <span className="font-mono text-xs">container :{p.containerPort}</span>
            )}
            <Button variant="ghost" size="icon" className="ml-auto size-7" aria-label="Remove port" onClick={() => remove(i)}><Trash2 /></Button>
          </div>
        ))}
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Mode">
          <Select value={mode} onChange={(e) => setMode(e.target.value)}>
            <option value="internal">Internal (mesh only)</option>
            <option value="tcp">TCP proxy</option>
          </Select>
        </Field>
        <Field label="Container port"><Input type="number" value={containerPort} onChange={(e) => setContainerPort(Number(e.target.value))} /></Field>
      </div>
      {mode === 'tcp' && (
        <p className="text-xs text-muted-foreground">
          One random public port is assigned for the whole cluster. Every ingress node listens on that same port and forwards to healthy replicas — connect to any ingress IP.
        </p>
      )}
      {mode === 'internal' && (
        <p className="text-xs text-muted-foreground">
          Reachable only inside the WireGuard mesh (e.g. other services via DNS). For a public website, add a domain below — no port mode required.
        </p>
      )}
      <div className="flex justify-end">
        <Button size="sm" variant="secondary" disabled={save.isPending || !containerPort} onClick={add}><Plus />Add port</Button>
      </div>
    </Section>
  )
}

function ScalingSection({ service, replicas: reps }: { service: Service; replicas: Replica[] }) {
  const spec = service.spec
  const [n, setN] = useState(spec?.replicas ?? 1)
  const [strategy, setStrategy] = useState(spec?.strategy ?? DeployStrategy.START_FIRST)
  const [machines, setMachines] = useState((spec?.placement?.machines ?? []).join(', '))
  const machinesQ = useMachines()
  const scale = useAction((replicas: number) => api.services.scaleService({ serviceId: service.id, replicas }), {
    invalidate: [keys.service(service.id), ['project'], ['replicas']],
    success: 'Scaled',
  })
  const save = useSpecUpdate(service)
  const global = spec?.mode === 2
  return (
    <Section title="Replicas & placement" description={`${reps.filter((r) => r.state === ReplicaState.RUNNING).length} running · mode ${spec?.mode === 2 ? 'global' : 'replicated'}. Stop/Start live on the Deployments tab.`}>
      <div className="flex items-end gap-2">
        <Field label="Replicas"><Input type="number" min={0} className="w-24" value={n} onChange={(e) => setN(Number(e.target.value))} disabled={global} /></Field>
        <Button size="sm" variant="secondary" disabled={n === spec?.replicas || scale.isPending || global} onClick={() => scale.mutate(n)}>Scale</Button>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Deploy strategy" hint="Stop-first is used for services with exclusive volumes.">
          <Select value={strategy} onChange={(e) => setStrategy(Number(e.target.value))}>
            <option value={DeployStrategy.START_FIRST}>start-first (zero downtime)</option>
            <option value={DeployStrategy.STOP_FIRST}>stop-first</option>
          </Select>
        </Field>
        <Field label="Pin to machines" hint={`Comma-separated names. Available: ${(machinesQ.data ?? []).map((m) => m.name).join(', ') || '—'}`}>
          <Input value={machines} onChange={(e) => setMachines(e.target.value)} placeholder="any machine" />
        </Field>
      </div>
      <div className="flex justify-end">
        <Button size="sm" variant="secondary" disabled={save.isPending} onClick={() => save.mutate({ mask: ['strategy', 'placement'], spec: { strategy, placement: { ...spec?.placement, machines: machines.split(',').map((x) => x.trim()).filter(Boolean) } } })}>Save</Button>
      </div>
    </Section>
  )
}

function ReschedulableSection({ service, replicas }: { service: Service; replicas: Replica[] }) {
  const save = useSpecUpdate(service)
  const on = !!service.spec?.reschedulable
  const hasLocalVolume = (service.spec?.volumes ?? []).some((v) => v.persistence !== 'backup-restore')
  const stranded = replicas.filter((r) => r.state === ReplicaState.STRANDED)
  return (
    <Section title="Reschedulable" icon={<RotateCw className="size-4" />}>
      <div className="flex items-start justify-between gap-4">
        <div className="text-sm text-muted-foreground">When the machine running a replica is down past the grace period (60s), replicas are started on another eligible machine.</div>
        <Switch checked={on} onCheckedChange={(c) => save.mutate({ mask: ['reschedulable'], spec: { reschedulable: c } })} aria-label="Reschedulable" disabled={save.isPending} />
      </div>
      {on && hasLocalVolume && (
        <p className="flex items-start gap-2 rounded-md border border-warning/40 bg-warning/10 p-2.5 text-xs"><AlertTriangle className="mt-0.5 size-3.5 shrink-0 text-warning" />This service has local volumes. It is only rescheduled if each volume is set to <code>backup-restore</code>; otherwise replicas are shown as “stranded”.</p>
      )}
      {stranded.map((r) => <p key={r.id} className="text-xs text-warning">Replica {r.id}: {r.message || 'stranded'}</p>)}
    </Section>
  )
}

function DomainsSection({ service, domains }: { service: Service; domains: Domain[] }) {
  const [host, setHost] = useState('')
  const [port, setPort] = useState(service.spec?.ports?.[0]?.containerPort || 8080)
  const [hints, setHints] = useState<string[]>([])
  const hasDefault = domains.some((d) => d.generated)
  const add = useAction(() => api.services.addDomain({ serviceId: service.id, hostname: host, port }), {
    invalidate: [keys.service(service.id), ['project']],
    success: 'Domain added',
    onSuccess: (r) => { setHost(''); setHints(r.dnsHints) },
  })
  const enableDefault = useAction(() => api.services.addDomain({ serviceId: service.id, hostname: '', port }), {
    invalidate: [keys.service(service.id), ['project']],
    success: 'Default HTTPS domain enabled',
    onSuccess: (r) => setHints(r.dnsHints),
  })
  const remove = useAction((id: string) => api.services.removeDomain({ domainId: id }), { invalidate: [keys.service(service.id), ['project']], success: 'Domain removed' })
  return (
    <Section
      title="Domains"
      icon={<Globe className="size-4" />}
      description="Public HTTPS via Caddy. Each domain points at a container port — TLS certificates are issued automatically."
    >
      <div className="divide-y rounded-md border">
        {domains.length === 0 && (
          <p className="p-3 text-sm text-muted-foreground">
            No public URL yet. Enable the default domain or add a custom hostname to expose this service over HTTPS.
          </p>
        )}
        {domains.map((d) => (
          <div key={d.id} className="flex items-center gap-2 p-2.5 text-sm">
            <a className="min-w-0 flex-1 truncate hover:underline" href={`https://${d.hostname}`} target="_blank" rel="noreferrer">{d.hostname}</a>
            <span className="font-mono text-xs text-muted-foreground">→ :{d.port}</span>
            {d.generated && <Badge>default</Badge>}
            <Badge tone={d.tlsState === 'issued' ? 'success' : 'warning'} title={tsDate(d.tlsExpiresAt)?.toLocaleDateString()}>{d.tlsState || 'pending'}</Badge>
            <Button variant="ghost" size="icon" className="size-7" aria-label={`Remove ${d.hostname}`} onClick={() => remove.mutate(d.id)}><Trash2 /></Button>
          </div>
        ))}
      </div>
      {!hasDefault && (
        <div className="flex flex-wrap items-end gap-2 rounded-md border border-dashed bg-muted/30 p-3">
          <Field label="Container port" hint="Where your app listens inside the container.">
            <Input className="w-28" type="number" value={port} onChange={(e) => setPort(Number(e.target.value))} />
          </Field>
          <Button size="sm" variant="secondary" disabled={!port || enableDefault.isPending} onClick={() => enableDefault.mutate(undefined)}>
            Enable default HTTPS
          </Button>
        </div>
      )}
      <form className="flex gap-2" onSubmit={(e) => { e.preventDefault(); add.mutate(undefined) }}>
        <Input value={host} onChange={(e) => setHost(e.target.value)} placeholder="app.example.com" aria-label="Custom hostname" />
        <Input className="w-24" type="number" value={port} onChange={(e) => setPort(Number(e.target.value))} aria-label="Container port" />
        <Button type="submit" size="sm" className="h-9" disabled={!host || !port || add.isPending}>Add domain</Button>
      </form>
      {hints.length > 0 && (
        <div className="rounded-md border bg-muted/40 p-2.5 text-xs"><div className="mb-1 font-medium">Point your DNS at this cluster:</div>{hints.map((h) => <div key={h}><code>{h}</code></div>)}</div>
      )}
    </Section>
  )
}

function VolumesSection({ service }: { service: Service }) {
  const vols = service.spec?.volumes ?? []
  const save = useSpecUpdate(service)
  const [editing, setEditing] = useState<number | 'new' | null>(null)
  const [draft, setDraft] = useState<VolumeDraft>(emptyVolume({ source: `${service.name}-data`, target: '/data' }))
  const [redeploy, setRedeploy] = useState(true)

  const openNew = () => {
    setDraft(emptyVolume({ source: `${service.name}-data`, target: '/data', persistence: 'local' }))
    setEditing('new')
  }
  const openEdit = (i: number) => {
    setDraft(volumeFromSpec(vols[i]))
    setEditing(i)
  }
  const commit = () => {
    if (!volumeValid(draft)) return
    const row = volumeToSpec(draft)
    const next: VolumeMount[] =
      editing === 'new' ? [...vols, row as VolumeMount]
        : typeof editing === 'number' ? vols.map((x, j) => (j === editing ? { ...x, ...row } : x))
          : vols
    save.mutate({ mask: ['volumes'], redeploy, spec: { volumes: next } }, { onSuccess: () => setEditing(null) })
  }
  const remove = (i: number) => {
    if (!confirm(`Remove volume “${vols[i].source}”? The Docker volume is not deleted from disk.`)) return
    save.mutate({ mask: ['volumes'], redeploy, spec: { volumes: vols.filter((_, j) => j !== i) } })
  }

  return (
    <Section
      title="Persistent storage"
      icon={<HardDrive className="size-4" />}
      description="Named Docker volumes mounted into this service. Create and manage them here — or attach one when creating a new service."
    >
      <div className="rounded-md border border-dashed bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
        Volumes live under <strong className="font-medium text-foreground">Settings → Persistent storage</strong> on each service.
        Use <em>backup &amp; restore</em> if the service should reschedule after a node failure (requires an S3 target under Settings → S3 Targets; schedule and retain are set per volume).
        Size limits are applied when the volume is first created.
      </div>

      <div className="divide-y rounded-md border">
        {vols.length === 0 && (
          <p className="p-3 text-sm text-muted-foreground">No volumes yet. Add persistent storage so databases and uploads survive container restarts.</p>
        )}
        {vols.map((v, i) => (
          <div key={`${v.source}-${i}`} className="flex flex-wrap items-center gap-2 p-2.5 text-sm">
            <div className="min-w-0 flex-1">
              <div className="truncate font-mono text-xs"><span className="text-foreground">{v.source}</span> <span className="text-muted-foreground">→</span> {v.target}</div>
              <div className="mt-1 flex flex-wrap gap-1.5">
                <Badge tone={v.persistence === 'backup-restore' ? 'info' : undefined}>
                  {v.persistence === 'backup-restore' ? 'backup-restore' : 'local'}
                </Badge>
                {v.persistence === 'backup-restore' && (
                  <Badge>{v.backupCron || '0 3 * * *'} · keep {v.backupRetain > 0 ? v.backupRetain : 7}</Badge>
                )}
                {!!v.sizeBytes && <Badge>{formatBytes(v.sizeBytes)}</Badge>}
                {v.readOnly && <Badge>read-only</Badge>}
              </div>
            </div>
            <Button variant="ghost" size="icon" className="size-7" aria-label={`Edit ${v.source}`} onClick={() => openEdit(i)}><Pencil /></Button>
            <Button variant="ghost" size="icon" className="size-7" aria-label={`Remove ${v.source}`} onClick={() => remove(i)}><Trash2 /></Button>
          </div>
        ))}
      </div>

      {editing !== null ? (
        <div className="space-y-3 rounded-md border p-3">
          <div className="text-sm font-medium">{editing === 'new' ? 'Add volume' : 'Edit volume'}</div>
          <VolumeFields value={draft} onChange={setDraft} />
          <label className="flex items-center gap-2 text-sm">
            <Switch checked={redeploy} onCheckedChange={setRedeploy} />
            Redeploy after save so mounts apply
          </label>
          <div className="flex justify-end gap-2">
            <Button type="button" size="sm" variant="ghost" onClick={() => setEditing(null)}>Cancel</Button>
            <Button type="button" size="sm" disabled={!volumeValid(draft) || save.isPending} onClick={commit}>
              {save.isPending ? 'Saving…' : editing === 'new' ? 'Add volume' : 'Save volume'}
            </Button>
          </div>
        </div>
      ) : (
        <Button type="button" size="sm" variant="secondary" onClick={openNew}><Plus />Add volume</Button>
      )}
    </Section>
  )
}

function ResourcesSection({ service }: { service: Service }) {
  const r = service.spec?.resources
  const [cpu, setCpu] = useState(Number(r?.cpuMillis ?? 0))
  const [mem, setMem] = useState(Math.round(Number(r?.memoryBytes ?? 0) / 1048576))
  const save = useSpecUpdate(service)
  return (
    <Section title="Resources" description={r?.memoryBytes ? `Current limit ${formatBytes(r.memoryBytes)} memory` : 'No limits set'}>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="CPU (millicores)" hint="1000 = one core, 0 = unlimited"><Input type="number" min={0} value={cpu} onChange={(e) => setCpu(Number(e.target.value))} /></Field>
        <Field label="Memory (MiB)" hint="0 = unlimited"><Input type="number" min={0} value={mem} onChange={(e) => setMem(Number(e.target.value))} /></Field>
      </div>
      <div className="flex justify-end"><Button size="sm" variant="secondary" disabled={save.isPending} onClick={() => save.mutate({ mask: ['resources'], redeploy: true, spec: { resources: { ...r, cpuMillis: BigInt(cpu), memoryBytes: BigInt(mem) * 1048576n } } })}>Save & redeploy</Button></div>
    </Section>
  )
}

function HealthSection({ service }: { service: Service }) {
  const h = service.spec?.healthCheck
  const [path, setPath] = useState(h?.httpPath ?? '')
  const [port, setPort] = useState(h?.httpPort ?? 0)
  const [interval, setIntervalS] = useState(h?.intervalSeconds ?? 10)
  const save = useSpecUpdate(service)
  return (
    <Section title="Health check" description="Gates rolling deploys; failures roll back automatically.">
      <div className="grid gap-3 sm:grid-cols-3">
        <Field label="HTTP path"><Input value={path} onChange={(e) => setPath(e.target.value)} placeholder="/healthz" /></Field>
        <Field label="Port"><Input type="number" value={port} onChange={(e) => setPort(Number(e.target.value))} /></Field>
        <Field label="Interval (s)"><Input type="number" value={interval} onChange={(e) => setIntervalS(Number(e.target.value))} /></Field>
      </div>
      <div className="flex justify-end"><Button size="sm" variant="secondary" disabled={save.isPending} onClick={() => save.mutate({ mask: ['health_check'], spec: { healthCheck: { ...h, httpPath: path, httpPort: port, intervalSeconds: interval } } })}>Save</Button></div>
    </Section>
  )
}

function DangerSection({ service }: { service: Service }) {
  const nav = useNavigate()
  const [confirm, setConfirm] = useState('')
  const restart = useAction(() => api.services.restartService({ serviceId: service.id }), { invalidate: [keys.service(service.id)], success: 'Restarting' })
  const del = useAction(() => api.services.deleteService({ serviceId: service.id, confirm: true }), {
    invalidate: [['project']],
    success: 'Service deleted',
    onSuccess: () => void nav({ to: '.', search: (p: Record<string, unknown>) => ({ ...p, service: undefined, tab: undefined }) as never }),
  })
  return (
    <Section title="Danger zone">
      <div className="flex items-center justify-between"><span className="text-sm">Restart all replicas</span><Button size="sm" variant="outline" onClick={() => restart.mutate(undefined)}>Restart</Button></div>
      <div className="flex items-end gap-2">
        <Field label={`Type “${service.name}” to delete`}><Input value={confirm} onChange={(e) => setConfirm(e.target.value)} /></Field>
        <Button size="sm" variant="destructive" disabled={confirm !== service.name || del.isPending} onClick={() => del.mutate(undefined)}><Trash2 />Delete service</Button>
      </div>
    </Section>
  )
}
