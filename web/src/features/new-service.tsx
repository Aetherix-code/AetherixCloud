import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Container, Database, GitBranch, HardDrive, ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogHeader } from '@/components/ui/dialog'
import { Field, Input, Select, Textarea } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { VolumeFields } from '@/features/volume-form'
import { api, orUnimplemented } from '@/lib/api'
import { DeployStrategy, PortProtocol, ServiceMode } from '@/gen/aetherix/v1/types_pb'
import { useAction } from '@/lib/queries'
import { emptyVolume, volumeToSpec, volumeValid, type VolumeDraft } from '@/lib/volumes'
import { cn } from '@/lib/utils'
import { parseEnvText } from '@/lib/refs'

export interface Template {
  id: string
  label: string
  image: string
  port: number
  volume?: string
  command?: string[]
  env: Record<string, string>
  blurb: string
}

export const TEMPLATES: Template[] = [
  { id: 'postgres', label: 'Postgres', image: 'postgres:16', port: 5432, volume: '/var/lib/postgresql/data', env: { POSTGRES_USER: 'app', POSTGRES_DB: 'app' }, blurb: 'Relational database with a data volume at /var/lib/postgresql/data.' },
  { id: 'redis', label: 'Redis', image: 'redis:7', port: 6379, volume: '/data', env: {}, blurb: 'In-memory store; persistence volume at /data.' },
  { id: 'mysql', label: 'MySQL', image: 'mysql:8', port: 3306, volume: '/var/lib/mysql', env: { MYSQL_DATABASE: 'app' }, blurb: 'MySQL 8 with a data volume at /var/lib/mysql.' },
  { id: 'minio', label: 'MinIO', image: 'minio/minio:latest', port: 9000, volume: '/data', command: ['server', '/data', '--console-address', ':9001'], env: { MINIO_ROOT_USER: 'admin' }, blurb: 'S3-compatible object storage; data at /data.' },
]

const secretFor = () => Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(16).padStart(2, '0')).join('')

type SourceKind = 'github' | 'image' | 'template'
type Step = 'source' | 'configure' | 'storage' | 'env' | 'review'

const STEPS: { id: Step; label: string }[] = [
  { id: 'source', label: 'Source' },
  { id: 'configure', label: 'Configure' },
  { id: 'storage', label: 'Storage' },
  { id: 'env', label: 'Env' },
  { id: 'review', label: 'Deploy' },
]

const SOURCES: { id: SourceKind; title: string; desc: string; icon: typeof GitBranch }[] = [
  { id: 'github', title: 'GitHub repo', desc: 'Build from a repository on every push.', icon: GitBranch },
  { id: 'image', title: 'Docker image', desc: 'Run an existing image from a registry.', icon: Container },
  { id: 'template', title: 'Template', desc: 'Postgres, Redis, MySQL, or MinIO with storage.', icon: Database },
]

export function NewServiceDialog({ projectId, environment, kind, template, onClose, onCreated }: {
  projectId: string
  environment?: string
  kind?: string
  template?: string
  onClose: () => void
  onCreated: (serviceId: string) => void
}) {
  const initialKind = (kind === 'image' || kind === 'template' || kind === 'github' ? kind : 'github') as SourceKind
  const [step, setStep] = useState<Step>(kind ? 'configure' : 'source')
  const [k, setK] = useState<SourceKind>(initialKind)
  const tpl = TEMPLATES.find((t) => t.id === template) ?? TEMPLATES[0]
  const [tplId, setTplId] = useState(tpl.id)
  const t = TEMPLATES.find((x) => x.id === tplId)!
  const [name, setName] = useState(kind === 'template' ? tpl.id : '')
  const [image, setImage] = useState('')
  const [repo, setRepo] = useState('')
  const [branch, setBranch] = useState('main')
  const [rootDir, setRootDir] = useState('')
  const [watchPaths, setWatchPaths] = useState('')
  const [dockerfile, setDockerfile] = useState('')
  const [exposeTcp, setExposeTcp] = useState(false)
  const [addVolume, setAddVolume] = useState(!!(kind === 'template' && tpl.volume))
  const [vol, setVol] = useState<VolumeDraft>(() =>
    emptyVolume({
      source: `${kind === 'template' ? tpl.id : 'app'}-data`,
      target: tpl.volume ?? '/data',
      persistence: 'local',
      sizeMiB: kind === 'template' ? 10240 : 0,
    }),
  )
  const [envText, setEnvText] = useState('')
  const repos = useQuery({ queryKey: ['github', 'repos'], retry: false, queryFn: () => orUnimplemented(api.github.listRepositories({ installationId: 0n, query: '' }).then((r) => r.repositories), null) })

  const pickSource = (next: SourceKind) => {
    setK(next)
    if (next === 'template') {
      setName(t.id)
      setAddVolume(!!t.volume)
      setExposeTcp(true)
      setVol(emptyVolume({ source: `${t.id}-data`, target: t.volume ?? '/data', persistence: 'local', sizeMiB: 10240 }))
      setEnvText('')
    } else if (next === 'image') {
      setAddVolume(false)
      setVol(emptyVolume({ source: 'app-data', target: '/data' }))
    } else {
      setAddVolume(false)
    }
    setStep('configure')
  }

  const onTemplateChange = (id: string) => {
    const next = TEMPLATES.find((x) => x.id === id)!
    setTplId(id)
    setName(id)
    setAddVolume(!!next.volume)
    setExposeTcp(true)
    setVol(emptyVolume({ source: `${id}-data`, target: next.volume ?? '/data', persistence: 'local', sizeMiB: 10240 }))
  }

  const onRootDirChange = (v: string) => {
    setRootDir(v)
    const root = v.trim().replace(/^\/+|\/+$/g, '')
    if (root && !watchPaths.trim()) setWatchPaths(`${root}/**`)
  }

  const envPairs = useMemo(() => parseEnvText(envText), [envText])
  const storageOk = !addVolume || volumeValid(vol)
  const configureOk = !!name && (k === 'github' ? !!repo : k === 'image' ? !!image : true)
  const canNext =
    step === 'source' ? !!k
      : step === 'configure' ? configureOk
        : step === 'storage' ? storageOk
          : true

  const create = useAction(
    () => {
      const base = { mode: ServiceMode.REPLICATED, strategy: DeployStrategy.START_FIRST, replicas: 1 }
      const extraEnv = Object.fromEntries(envPairs.map((p) => [p.key, p.value]))
      const volumes = addVolume && volumeValid(vol) ? [volumeToSpec(vol)] : []
      const root = rootDir.trim().replace(/^\/+|\/+$/g, '')
      const watch = watchPaths.split(',').map((x) => x.trim()).filter(Boolean)
      const githubBuild = {
        builder: '',
        context: root,
        dockerfile: dockerfile.trim(),
        watchPaths: watch.length ? watch : root ? [`${root}/**`] : [],
      }
      const tcpPorts = exposeTcp
        ? [{ containerPort: t.port, protocol: PortProtocol.TCP, mode: 'tcp' as const, publishedPort: 0 }]
        : [{ containerPort: t.port, protocol: PortProtocol.TCP, mode: 'internal' as const }]
      const spec =
        k === 'github'
          ? { ...base, source: { github: { repo, branch, installationId: repos.data?.find((r) => r.fullName === repo)?.installationId ?? 0n } }, build: githubBuild, volumes, env: extraEnv }
          : k === 'image'
            ? { ...base, source: { image }, image, volumes, env: extraEnv }
            : {
                ...base,
                strategy: DeployStrategy.STOP_FIRST,
                source: { image: t.image },
                image: t.image,
                command: t.command,
                ports: tcpPorts,
                volumes: t.volume
                  ? [volumeToSpec(addVolume && volumeValid(vol) ? vol : emptyVolume({ source: `${name}-data`, target: t.volume, persistence: 'local', sizeMiB: 10240 }))]
                  : volumes,
                env: {
                  ...t.env,
                  ...extraEnv,
                  ...(t.id === 'redis' ? {} : { [t.id === 'minio' ? 'MINIO_ROOT_PASSWORD' : t.id === 'mysql' ? 'MYSQL_ROOT_PASSWORD' : 'POSTGRES_PASSWORD']: secretFor() }),
                },
              }
      // GitHub sources have no image yet: CreateService enqueues a build when deploy=true.
      return api.services.createService({ project: projectId, environment: environment ?? '', name, spec, deploy: true })
    },
    { invalidate: [['project']], success: k === 'github' ? 'Service created; build queued' : 'Service created', onSuccess: (r) => r.service && onCreated(r.service.id) },
  )

  const stepIdx = STEPS.findIndex((s) => s.id === step)
  const goNext = () => {
    if (!canNext) return
    const next = STEPS[stepIdx + 1]
    if (next) setStep(next.id)
  }
  const goBack = () => {
    const prev = STEPS[stepIdx - 1]
    if (prev) setStep(prev.id)
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl p-0 sm:max-w-2xl">
        <div className="border-b px-5 pb-4 pt-5">
          <div className="pr-6">
            <DialogHeader title="New service" description="Deploy from GitHub, a Docker image, or a database template — with optional persistent storage." />
          </div>
          <ol className="flex flex-wrap gap-1.5" aria-label="Steps">
            {STEPS.map((s, i) => (
              <li key={s.id}>
                <button
                  type="button"
                  className={cn(
                    'rounded-md px-2.5 py-1 text-xs font-medium transition-colors',
                    s.id === step ? 'bg-primary text-primary-foreground' : i < stepIdx ? 'bg-muted text-foreground hover:bg-muted/80' : 'text-muted-foreground',
                  )}
                  onClick={() => { if (i <= stepIdx || (i === stepIdx + 1 && canNext)) setStep(s.id) }}
                >
                  {i + 1}. {s.label}
                </button>
              </li>
            ))}
          </ol>
        </div>

        <div className="max-h-[min(60vh,520px)] space-y-4 overflow-y-auto px-5 py-4">
          {step === 'source' && (
            <div className="grid gap-2 sm:grid-cols-3">
              {SOURCES.map((s) => {
                const Icon = s.icon
                const active = k === s.id
                return (
                  <button
                    key={s.id}
                    type="button"
                    onClick={() => pickSource(s.id)}
                    className={cn(
                      'flex flex-col items-start gap-2 rounded-lg border p-3 text-left transition-colors hover:bg-muted/50',
                      active && 'border-primary bg-accent/40',
                    )}
                  >
                    <Icon className="size-5 text-primary" />
                    <div className="text-sm font-medium">{s.title}</div>
                    <p className="text-xs text-muted-foreground">{s.desc}</p>
                  </button>
                )
              })}
            </div>
          )}

          {step === 'configure' && (
            <div className="space-y-4">
              {k === 'github' && (
                <>
                  <Field label="Repository" hint={repos.data === null ? 'GitHub App is not configured yet — enter owner/name manually.' : undefined}>
                    {repos.data?.length ? (
                      <Select value={repo} onChange={(e) => { setRepo(e.target.value); if (!name) setName(e.target.value.split('/')[1] ?? '') }}>
                        <option value="">Select a repository…</option>
                        {repos.data.map((r) => <option key={r.fullName} value={r.fullName}>{r.fullName}</option>)}
                      </Select>
                    ) : (
                      <Input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="owner/name" autoFocus />
                    )}
                  </Field>
                  <Field label="Branch"><Input value={branch} onChange={(e) => setBranch(e.target.value)} /></Field>
                  <div className="rounded-lg border border-dashed p-3 space-y-3">
                    <div className="text-sm font-medium">Monorepo</div>
                    <p className="text-xs text-muted-foreground">If this app lives in a subdirectory, set the root directory. Watch paths control which pushes rebuild only this service.</p>
                    <Field label="Root directory" hint="e.g. apps/api — leave empty for the repo root">
                      <Input value={rootDir} onChange={(e) => onRootDirChange(e.target.value)} placeholder="apps/web" className="font-mono text-xs" />
                    </Field>
                    <Field label="Dockerfile path" hint="Relative to the root directory">
                      <Input value={dockerfile} onChange={(e) => setDockerfile(e.target.value)} placeholder="Dockerfile" className="font-mono text-xs" />
                    </Field>
                    <Field label="Watch paths" hint="Comma-separated globs. Defaults to <root>/** when root is set.">
                      <Input value={watchPaths} onChange={(e) => setWatchPaths(e.target.value)} placeholder="apps/web/**, packages/ui/**" className="font-mono text-xs" />
                    </Field>
                  </div>
                </>
              )}
              {k === 'image' && (
                <Field label="Image">
                  <Input value={image} onChange={(e) => setImage(e.target.value)} placeholder="ghcr.io/org/app:tag" autoFocus />
                </Field>
              )}
              {k === 'template' && (
                <div className="space-y-3">
                  <div className="grid gap-2 sm:grid-cols-2">
                    {TEMPLATES.map((x) => (
                      <button
                        key={x.id}
                        type="button"
                        onClick={() => onTemplateChange(x.id)}
                        className={cn(
                          'rounded-lg border p-3 text-left transition-colors hover:bg-muted/50',
                          tplId === x.id && 'border-primary bg-accent/40',
                        )}
                      >
                        <div className="text-sm font-medium">{x.label}</div>
                        <p className="mt-1 text-xs text-muted-foreground">{x.blurb}</p>
                      </button>
                    ))}
                  </div>
                  <label className="flex items-start gap-2.5 text-sm">
                    <Switch checked={exposeTcp} onCheckedChange={setExposeTcp} />
                    <span>
                      <span className="font-medium">Expose via TCP proxy</span>
                      <span className="mt-0.5 block text-xs text-muted-foreground">Assigns one random port on every ingress node so clients can connect from outside the mesh.</span>
                    </span>
                  </label>
                  <p className="text-xs text-muted-foreground">Credentials are generated automatically. A data volume is attached on the next step.</p>
                </div>
              )}
              <Field label="Service name">
                <Input value={name} onChange={(e) => setName(e.target.value)} pattern="[a-z0-9][a-z0-9-]*" title="lowercase letters, digits and dashes" required autoFocus={k === 'template'} />
              </Field>
            </div>
          )}

          {step === 'storage' && (
            <div className="space-y-4">
              <div className="flex items-start justify-between gap-4 rounded-lg border p-3">
                <div className="min-w-0">
                  <div className="flex items-center gap-2 text-sm font-medium"><HardDrive className="size-4" />Add persistent storage</div>
                  <p className="mt-1 text-xs text-muted-foreground">
                    Named Docker volumes keep data across deploys. Manage them later under the service’s <strong className="font-medium text-foreground">Settings → Persistent storage</strong>.
                    {k === 'template' && t.volume ? ' Database templates include a volume by default.' : ''}
                  </p>
                </div>
                <Switch
                  checked={addVolume}
                  onCheckedChange={(c) => {
                    setAddVolume(c)
                    if (c && !vol.source) setVol(emptyVolume({ source: `${name || 'app'}-data`, target: k === 'template' && t.volume ? t.volume : '/data', sizeMiB: k === 'template' ? 10240 : 0 }))
                  }}
                  aria-label="Add persistent storage"
                  disabled={k === 'template' && !!t.volume}
                />
              </div>
              {addVolume && (
                <VolumeFields
                  value={vol}
                  onChange={(v) => setVol({ ...v, source: v.source || `${name || 'app'}-data` })}
                  nameHint="Defaults to <service>-data. Docker namespaces it per service on the node."
                />
              )}
            </div>
          )}

          {step === 'env' && (
            <div className="space-y-3">
              <Field label="Environment variables" hint={k === 'template' ? 'Template defaults and generated secrets are added automatically. Extra KEY=value pairs here are merged in.' : 'Optional KEY=value pairs (one per line). Secrets can be refined after deploy under Variables.'}>
                <Textarea rows={8} value={envText} onChange={(e) => setEnvText(e.target.value)} placeholder={'KEY=value\nANOTHER=1'} className="text-xs" />
              </Field>
            </div>
          )}

          {step === 'review' && (
            <dl className="space-y-2 rounded-lg border bg-muted/30 p-3 text-sm">
              <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Name</dt><dd className="font-medium">{name}</dd></div>
              <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Source</dt><dd className="truncate text-right font-mono text-xs">{k === 'github' ? `${repo}@${branch}` : k === 'image' ? image : `${t.label} (${t.image})`}</dd></div>
              {k === 'github' && !!rootDir.trim() && (
                <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Root dir</dt><dd className="font-mono text-xs">{rootDir.trim()}</dd></div>
              )}
              {k === 'github' && !!watchPaths.trim() && (
                <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Watch</dt><dd className="truncate text-right font-mono text-xs">{watchPaths}</dd></div>
              )}
              {k === 'template' && exposeTcp && (
                <div className="flex justify-between gap-4"><dt className="text-muted-foreground">TCP proxy</dt><dd className="text-xs">random ingress port</dd></div>
              )}
              <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Storage</dt><dd className="text-right text-xs">{addVolume ? `${vol.source} → ${vol.target} (${vol.persistence}${vol.sizeMiB ? `, ${vol.sizeMiB} MiB` : ''})` : 'None'}</dd></div>
              <div className="flex justify-between gap-4"><dt className="text-muted-foreground">Env vars</dt><dd>{envPairs.length + (k === 'template' ? Object.keys(t.env).length + (t.id === 'redis' ? 0 : 1) : 0)}</dd></div>
            </dl>
          )}
        </div>

        <div className="flex items-center justify-between gap-2 border-t px-5 py-3">
          <Button type="button" variant="ghost" onClick={stepIdx === 0 ? onClose : goBack}>
            {stepIdx === 0 ? 'Cancel' : <><ChevronLeft />Back</>}
          </Button>
          {step === 'review' ? (
            <Button type="button" disabled={!configureOk || !storageOk || create.isPending} onClick={() => create.mutate(undefined)}>
              {create.isPending ? 'Creating…' : 'Create & deploy'}
            </Button>
          ) : (
            <Button type="button" disabled={!canNext} onClick={goNext}>Continue <ChevronRight /></Button>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
