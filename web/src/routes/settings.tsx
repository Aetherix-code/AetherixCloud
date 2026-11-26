import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ArrowUpCircle, Bot, Check, Copy, Download, Pencil, Plus, RotateCcw, Trash2, Upload, UserPlus } from 'lucide-react'
import { Link } from '@tanstack/react-router'
import { Page, PageHeader } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogHeader } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { Empty, ListSkeleton, NotAvailable, QueryError } from '@/components/ui/states'
import { toast } from '@/components/ui/toast'
import { Role } from '@/gen/aetherix/v1/types_pb'
import { api, errMessage, isMock, isUnimplemented } from '@/lib/api'
import { Checkbox, CheckboxField } from '@/components/ui/checkbox'
import {
  createS3Target, deleteS3Target, fetchBackups, fetchBuildImageArchive, fetchS3Targets,
  restoreBackup, restoreClusterSnapshot, runBackupNow, runClusterBackupNow,
  saveBuildImageArchive, saveClusterBackupSchedule, updateS3Target,
  type BackupRun, type BuildImageArchiveConfig, type ClusterBackupSchedule, type S3Target,
} from '@/lib/extras'
import { keys, useAction, useGithubApp, useGrants, useStatus, useTokens, useUsers } from '@/lib/queries'
import { userRoleLabel } from '@/lib/status'
import { copy, formatBytes, timeAgo, tsDate } from '@/lib/utils'

function CopyBox({ text, label }: { text: string; label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <div className="relative">
      {label && <div className="mb-1 text-xs font-medium text-muted-foreground">{label}</div>}
      <pre className="overflow-x-auto rounded-md border bg-muted p-3 pr-12 font-mono text-xs whitespace-pre-wrap break-all">{text}</pre>
      <Button size="icon" variant="ghost" className="absolute right-1.5 top-1.5 size-7" aria-label="Copy" style={{ top: label ? 26 : 6 }} onClick={() => { void copy(text); setDone(true); setTimeout(() => setDone(false), 1500) }}>
        {done ? <Check /> : <Copy />}
      </Button>
    </div>
  )
}

// ---------------------------------------------------------------- Users & roles
const roles = [Role.OWNER, Role.ADMIN, Role.DEVELOPER, Role.VIEWER]

export function UsersSettings() {
  const q = useUsers()
  const [open, setOpen] = useState(false)
  const [f, setF] = useState({ email: '', name: '', password: '', role: Role.DEVELOPER })
  const create = useAction(() => api.auth.createUser(f), { invalidate: [keys.users], success: 'User created', onSuccess: () => { setOpen(false); setF({ email: '', name: '', password: '', role: Role.DEVELOPER }) } })
  const update = useAction((a: { id: string; role: Role }) => api.auth.updateUser({ userId: a.id, role: a.role }), { invalidate: [keys.users], success: 'Role updated' })
  const del = useAction((id: string) => api.auth.deleteUser({ userId: id }), { invalidate: [keys.users], success: 'User removed' })
  return (
    <Page>
      <PageHeader title="Users & roles" hideTitle description="Invite teammates and assign Owner, Admin, Developer or Viewer. Role changes are recorded in the Audit log and apply to the dashboard, API tokens and AI agents." actions={q.data ? <Button onClick={() => setOpen(true)}><UserPlus />Add user</Button> : undefined} />
      {q.isLoading ? <ListSkeleton /> : q.error ? <QueryError error={q.error} what="users" /> : q.data === null ? <NotAvailable what="User management" /> : (
        <Card className="divide-y">
          {q.data!.map((u) => (
            <div key={u.id} className="flex items-center gap-3 p-3 text-sm">
              <div className="grid size-8 place-items-center rounded-full bg-accent text-xs font-semibold">{(u.name || u.email)[0].toUpperCase()}</div>
              <div className="min-w-0 flex-1"><div className="truncate font-medium">{u.name}</div><div className="truncate text-xs text-muted-foreground">{u.email}</div></div>
              <Select className="w-36" value={u.role} onChange={(e) => update.mutate({ id: u.id, role: Number(e.target.value) })} aria-label={`Role for ${u.email}`}>
                {roles.map((r) => <option key={r} value={r}>{userRoleLabel[r]}</option>)}
              </Select>
              <Button variant="ghost" size="icon" aria-label={`Remove ${u.email}`} onClick={() => del.mutate(u.id)}><Trash2 /></Button>
            </div>
          ))}
        </Card>
      )}
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader title="Add user" />
          <form className="space-y-3" onSubmit={(e) => { e.preventDefault(); create.mutate(undefined) }}>
            <Field label="Email"><Input type="email" required value={f.email} onChange={(e) => setF({ ...f, email: e.target.value })} /></Field>
            <Field label="Name"><Input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></Field>
            <Field label="Password"><Input type="password" required minLength={8} value={f.password} onChange={(e) => setF({ ...f, password: e.target.value })} /></Field>
            <Field label="Role"><Select value={f.role} onChange={(e) => setF({ ...f, role: Number(e.target.value) })}>{roles.map((r) => <option key={r} value={r}>{userRoleLabel[r]}</option>)}</Select></Field>
            <div className="flex justify-end"><Button type="submit" disabled={create.isPending}>Create user</Button></div>
          </form>
        </DialogContent>
      </Dialog>
    </Page>
  )
}

// ---------------------------------------------------------------- GitHub App
export function GithubSettings() {
  const app = useGithubApp()
  const [base, setBase] = useState(window.location.origin)
  const [org, setOrg] = useState('')
  const [webhook, setWebhook] = useState('')
  const formRef = useRef<HTMLFormElement>(null)
  const [manifest, setManifest] = useState<{ json: string; postUrl: string; state: string } | null>(null)
  const installs = useRealInstallations(!!app.data?.configured)

  const complete = useAction((a: { code: string; state: string }) => api.github.completeManifestSetup(a), { invalidate: [keys.ghApp, keys.installations], success: 'GitHub App configured' })
  // GitHub redirects back here with ?code=&state= after the manifest is approved.
  const handled = useRef(false)
  useEffect(() => {
    const p = new URLSearchParams(window.location.search)
    const code = p.get('code')
    if (code && !handled.current) {
      handled.current = true
      complete.mutate({ code, state: p.get('state') ?? '' })
      window.history.replaceState({}, '', window.location.pathname)
    }
  }, [complete])

  const begin = useAction(() => api.github.beginManifestSetup({ baseUrl: base, organization: org }), {
    onSuccess: (r) => setManifest({ json: r.manifestJson, postUrl: r.postUrl, state: r.state }),
  })
  const setHook = useAction(() => api.github.setWebhookUrl({ url: webhook }), { invalidate: [keys.ghApp], success: 'Webhook URL updated' })

  useEffect(() => {
    if (manifest && !isMock) formRef.current?.submit()
  }, [manifest])

  return (
    <Page>
      <PageHeader title="GitHub App" hideTitle description="Deploy on push, commit statuses and PR previews through a GitHub App created with the manifest flow." />
      {app.isLoading ? <ListSkeleton rows={2} /> : app.error ? <QueryError error={app.error} what="the GitHub App" /> : app.data === null ? (
        <NotAvailable what="The GitHub integration" />
      ) : app.data?.configured ? (
        <div className="space-y-4">
          <Card>
            <CardHeader><CardTitle className="flex items-center gap-2">Connected <Badge tone="success">app {String(app.data.appId)}</Badge></CardTitle><CardDescription>{app.data.slug}</CardDescription></CardHeader>
            <CardContent className="space-y-4">
              <CopyBox label="Webhook URL (served by every node)" text={app.data.webhookUrl} />
              <form className="flex gap-2" onSubmit={(e) => { e.preventDefault(); setHook.mutate(undefined) }}>
                <Input value={webhook} onChange={(e) => setWebhook(e.target.value)} placeholder="Override webhook URL (e.g. a tunnel for local dev)" aria-label="Webhook URL override" />
                <Button type="submit" variant="secondary" className="h-9" disabled={!webhook}>Update</Button>
              </form>
              <Button asChild variant="outline"><a href={`${app.data.htmlUrl}/installations/new`} target="_blank" rel="noreferrer">Install on an account</a></Button>
            </CardContent>
          </Card>
          <Card>
            <CardHeader><CardTitle>Installations</CardTitle></CardHeader>
            <CardContent>
              {!installs.data?.length ? <p className="text-sm text-muted-foreground">No installations yet.</p> : installs.data.map((i) => <div key={String(i.id)} className="flex items-center gap-2 text-sm"><span className="font-medium">{i.account}</span><Badge>{i.accountType}</Badge></div>)}
            </CardContent>
          </Card>
        </div>
      ) : (
        <Card>
          <CardHeader><CardTitle>Create the GitHub App</CardTitle><CardDescription>Three steps: describe the app, approve it on GitHub, then install it on your repositories.</CardDescription></CardHeader>
          <CardContent className="space-y-4">
            <ol className="list-decimal space-y-1 pl-5 text-sm text-muted-foreground">
              <li>Confirm the public dashboard URL and (optionally) a GitHub organization.</li>
              <li>You are sent to GitHub to approve the generated manifest.</li>
              <li>GitHub redirects back here; the App ID, private key and webhook secret are stored encrypted.</li>
            </ol>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Public dashboard URL" hint="Webhooks are delivered to <url>/api/github/webhook"><Input value={base} onChange={(e) => setBase(e.target.value)} /></Field>
              <Field label="Organization (optional)"><Input value={org} onChange={(e) => setOrg(e.target.value)} placeholder="personal account" /></Field>
            </div>
            <Button onClick={() => begin.mutate(undefined)} disabled={begin.isPending}>Create GitHub App</Button>
            {manifest && (
              <>
                <form ref={formRef} action={`${manifest.postUrl}?state=${encodeURIComponent(manifest.state)}`} method="post" target={isMock ? '_blank' : undefined}>
                  <input type="hidden" name="manifest" value={manifest.json} />
                </form>
                <CopyBox label="Manifest" text={manifest.json} />
                {isMock ? (
                  <Button variant="secondary" onClick={() => complete.mutate({ code: 'mock-code', state: manifest.state })}>Simulate GitHub approval (mock)</Button>
                ) : (
                  <Button variant="secondary" onClick={() => formRef.current?.submit()}>Continue to GitHub</Button>
                )}
              </>
            )}
          </CardContent>
        </Card>
      )}
    </Page>
  )
}

function useRealInstallations(enabled: boolean) {
  return useQuery({ queryKey: keys.installations, enabled, retry: false, queryFn: async () => (await api.github.listInstallations({})).installations })
}

// ---------------------------------------------------------------- AI agents
const SCOPES = ['read', 'deploy', 'admin']

export function AgentsSettings() {
  const url = `${window.location.origin}/mcp`
  const tokens = useTokens()
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [scopes, setScopes] = useState<string[]>(['read'])
  const [ttl, setTtl] = useState(0)
  const [secret, setSecret] = useState('')
  const create = useAction(() => api.auth.createApiToken({ name, scopes, ttlSeconds: ttl }), { invalidate: [keys.tokens], onSuccess: (r) => { setSecret(r.plaintext); setName('') } })
  const revoke = useAction((id: string) => api.auth.revokeApiToken({ tokenId: id }), { invalidate: [keys.tokens], success: 'Token revoked' })
  const grants = useGrants()
  const revokeGrant = useAction((id: string) => api.auth.revokeOAuthGrant({ grantId: id }), { invalidate: [keys.grants], success: 'Grant revoked' })
  const token = secret || '<API_TOKEN>'
  const cursorCfg = JSON.stringify({ mcpServers: { aetherix: { url, headers: { Authorization: `Bearer ${token}` } } } }, null, 2)
  const claudeCfg = JSON.stringify({ mcpServers: { aetherix: { command: 'ax', args: ['mcp'] } } }, null, 2)
  return (
    <Page>
      <PageHeader title="AI agents" hideTitle description="Let ChatGPT, Claude or Cursor operate the cluster over MCP, with your RBAC role and scoped tokens." />
      <div className="space-y-4">
        <Card>
          <CardHeader><CardTitle className="flex items-center gap-2"><Bot className="size-4" />Connector URL</CardTitle><CardDescription>Add as a custom connector in ChatGPT or Claude and sign in with your AetherixCloud account (OAuth 2.1). Served by every node.</CardDescription></CardHeader>
          <CardContent><CopyBox text={url} /></CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Copy-paste config</CardTitle></CardHeader>
          <CardContent className="grid gap-4 md:grid-cols-2">
            <CopyBox label="Cursor (remote, with token)" text={cursorCfg} />
            <CopyBox label="Claude Desktop (stdio via ax)" text={claudeCfg} />
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Active OAuth grants</CardTitle><CardDescription>Connectors you authorized through the consent screen.</CardDescription></CardHeader>
          <CardContent>
            {grants.isLoading ? <ListSkeleton rows={2} /> : grants.error ? <QueryError error={grants.error} what="OAuth grants" /> : grants.data === null ? <NotAvailable what="OAuth grants" /> : !grants.data?.length ? (
              <p className="text-sm text-muted-foreground">No connectors have been authorized yet.</p>
            ) : (
              <div className="divide-y">
                {grants.data!.map((g) => (
                  <div key={g.id} className="flex flex-wrap items-center gap-2 py-2 text-sm">
                    <span className="font-medium">{g.clientName || g.clientId}</span>
                    {g.scopes.map((s) => <Badge key={s}>{s}</Badge>)}
                    <span className="ml-auto text-xs text-muted-foreground">{g.lastUsedAt ? `used ${timeAgo(g.lastUsedAt)}` : 'never used'} · granted {timeAgo(g.createdAt)}</span>
                    <Button size="sm" variant="outline" onClick={() => revokeGrant.mutate(g.id)} disabled={revokeGrant.isPending}>Revoke</Button>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="flex-row items-center justify-between"><div><CardTitle>API tokens</CardTitle><CardDescription>Scoped bearer tokens for scripts, CI and local agents.</CardDescription></div>{tokens.data !== null && <Button size="sm" onClick={() => { setOpen(true); setSecret('') }}>New token</Button>}</CardHeader>
          <CardContent>
            {tokens.isLoading ? <ListSkeleton rows={2} /> : tokens.error ? <QueryError error={tokens.error} what="tokens" /> : tokens.data === null ? <NotAvailable what="Token management" /> : !tokens.data?.length ? <Empty title="No tokens" /> : (
              <div className="divide-y">
                {tokens.data!.map((t) => (
                  <div key={t.id} className="flex items-center gap-2 py-2 text-sm">
                    <span className="font-medium">{t.name}</span>{t.scopes.map((s) => <Badge key={s} tone={s === 'admin' ? 'danger' : s === 'deploy' ? 'warning' : 'neutral'}>{s}</Badge>)}
                    <span className="ml-auto text-xs text-muted-foreground">{t.lastUsedAt ? `used ${timeAgo(t.lastUsedAt)}` : 'never used'}{t.expiresAt ? ` · expires ${tsDate(t.expiresAt)?.toLocaleDateString()}` : ''}</span>
                    <Button variant="ghost" size="icon" aria-label={`Revoke ${t.name}`} onClick={() => revoke.mutate(t.id)}><Trash2 /></Button>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      </div>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader title="New API token" description="The token is shown once." />
          {secret ? (
            <div className="space-y-3"><CopyBox text={secret} /><div className="flex justify-end"><Button onClick={() => setOpen(false)}>Done</Button></div></div>
          ) : (
            <form className="space-y-3" onSubmit={(e) => { e.preventDefault(); create.mutate(undefined) }}>
              <Field label="Name"><Input required value={name} onChange={(e) => setName(e.target.value)} placeholder="cursor-mcp" /></Field>
              <div className="space-y-1.5"><span className="text-xs font-medium text-muted-foreground">Scopes</span><div className="flex flex-wrap gap-4">{SCOPES.map((s) => <CheckboxField key={s} checked={scopes.includes(s)} onCheckedChange={(on) => setScopes(on ? [...scopes, s] : scopes.filter((x) => x !== s))}>{s}</CheckboxField>)}</div></div>
              <Field label="Expires"><Select value={ttl} onChange={(e) => setTtl(Number(e.target.value))}><option value={0}>Never</option><option value={86400}>1 day</option><option value={2592000}>30 days</option><option value={31536000}>1 year</option></Select></Field>
              <div className="flex justify-end"><Button type="submit" disabled={!scopes.length || create.isPending}>Create token</Button></div>
            </form>
          )}
        </DialogContent>
      </Dialog>
    </Page>
  )
}

// ---------------------------------------------------------------- S3 Targets
const emptyTarget = (): Omit<S3Target, 'id' | 'created_at'> => ({
  name: '', endpoint: '', region: 'us-east-1', bucket: '', access_key_id: '',
  secret_access_key: '', prefix: 'aetherix/', path_style: true,
})

export function S3TargetsSettings() {
  const q = useQuery({ queryKey: ['s3-targets'], queryFn: fetchS3Targets, retry: false, enabled: !isMock })
  const imgQ = useQuery({ queryKey: ['build-images'], queryFn: fetchBuildImageArchive, retry: false, enabled: !isMock })
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<S3Target | null>(null)
  const [form, setForm] = useState(emptyTarget())
  const [img, setImg] = useState<BuildImageArchiveConfig>({ enabled: false, target_id: '', retain: 5 })
  useEffect(() => {
    if (imgQ.data) setImg({ ...imgQ.data })
  }, [imgQ.data])

  function startCreate() {
    setEditing(null)
    setForm(emptyTarget())
    setOpen(true)
  }
  function startEdit(t: S3Target) {
    setEditing(t)
    setForm({
      name: t.name, endpoint: t.endpoint, region: t.region || 'us-east-1', bucket: t.bucket,
      access_key_id: t.access_key_id, secret_access_key: t.secret_access_key || '••••••••',
      prefix: t.prefix || 'aetherix/', path_style: t.path_style,
    })
    setOpen(true)
  }

  const save = useAction(
    async () => {
      if (editing) await updateS3Target(editing.id, form)
      else await createS3Target(form)
    },
    { invalidate: [['s3-targets'], ['backups']], success: editing ? 'Target updated' : 'Target created', onSuccess: () => setOpen(false) },
  )
  const del = useAction((id: string) => deleteS3Target(id), { invalidate: [['s3-targets'], ['backups']], success: 'Target removed' })
  const saveImg = useAction(() => saveBuildImageArchive(img), { invalidate: [['build-images']], success: 'Build image settings saved' })
  const list = q.data?.targets ?? []

  return (
    <Page>
      <PageHeader
        title="S3 Targets"
        hideTitle
        description="Named S3-compatible destinations for volume backups, cluster snapshots, and optional build-image archives."
        actions={!q.isError ? <Button onClick={startCreate}><Plus />Add target</Button> : undefined}
      />
      <div className="space-y-4">
        {q.isLoading ? <ListSkeleton /> : q.isError ? (
          <p className="text-sm text-muted-foreground">S3 targets API unavailable on this node.</p>
        ) : !(q.data?.targets.length) ? (
          <Empty title="No S3 targets">
            <p className="mb-3">Add MinIO, AWS S3, R2, or any S3-compatible endpoint.</p>
            <Button onClick={startCreate}><Plus />Add target</Button>
          </Empty>
        ) : (
          <Card className="divide-y">
            {q.data!.targets.map((t) => (
              <div key={t.id} className="flex items-center gap-3 p-3 text-sm">
                <div className="min-w-0 flex-1">
                  <div className="font-medium">{t.name}</div>
                  <div className="truncate text-xs text-muted-foreground">{t.endpoint} · {t.bucket}{t.prefix ? ` / ${t.prefix}` : ''}</div>
                </div>
                <Button variant="ghost" size="icon" aria-label={`Edit ${t.name}`} onClick={() => startEdit(t)}><Pencil /></Button>
                <Button variant="ghost" size="icon" aria-label={`Remove ${t.name}`} onClick={() => { if (confirm(`Remove target “${t.name}”?`)) del.mutate(t.id) }}><Trash2 /></Button>
              </div>
            ))}
          </Card>
        )}

        <Card>
          <CardHeader>
            <CardTitle>Build images</CardTitle>
            <CardDescription>
              Cluster-wide: after each successful build, archive the image to S3 and prune older local registry tags.
              Keeps disk usage down on builders while retaining recent images for rollback. The currently deployed image stays local.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {imgQ.isError ? <p className="text-sm text-muted-foreground">Build image settings unavailable on this node.</p> : (
              <form className="space-y-3" onSubmit={(e) => { e.preventDefault(); saveImg.mutate(undefined) }}>
                <CheckboxField checked={img.enabled} onCheckedChange={(on) => setImg({ ...img, enabled: on })}>
                  Archive build images to S3
                </CheckboxField>
                {img.enabled && (
                  <div className="grid gap-3 sm:grid-cols-2">
                    <Field label="S3 target">
                      <Select value={img.target_id} onChange={(e) => setImg({ ...img, target_id: e.target.value })}>
                        <option value="">Default target</option>
                        {list.map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
                      </Select>
                    </Field>
                    <Field label="Retain per app" hint="S3 archives and local tags kept per service.">
                      <Input type="number" min={1} value={img.retain} onChange={(e) => setImg({ ...img, retain: Math.max(1, Number(e.target.value) || 5) })} />
                    </Field>
                  </div>
                )}
                <Button type="submit" size="sm" disabled={saveImg.isPending}>Save</Button>
              </form>
            )}
          </CardContent>
        </Card>
      </div>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader title={editing ? 'Edit S3 target' : 'Add S3 target'} />
          <form className="space-y-3" onSubmit={(e) => { e.preventDefault(); save.mutate(undefined) }}>
            <Field label="Name"><Input required value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="Primary MinIO" /></Field>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Endpoint"><Input required value={form.endpoint} onChange={(e) => setForm({ ...form, endpoint: e.target.value })} placeholder="https://s3.amazonaws.com" /></Field>
              <Field label="Region"><Input value={form.region} onChange={(e) => setForm({ ...form, region: e.target.value })} /></Field>
              <Field label="Bucket"><Input required value={form.bucket} onChange={(e) => setForm({ ...form, bucket: e.target.value })} /></Field>
              <Field label="Prefix"><Input value={form.prefix} onChange={(e) => setForm({ ...form, prefix: e.target.value })} /></Field>
              <Field label="Access key"><Input required value={form.access_key_id} onChange={(e) => setForm({ ...form, access_key_id: e.target.value })} autoComplete="off" /></Field>
              <Field label="Secret key"><Input type="password" required={!editing} value={form.secret_access_key} onChange={(e) => setForm({ ...form, secret_access_key: e.target.value })} autoComplete="off" /></Field>
            </div>
            <CheckboxField checked={form.path_style} onCheckedChange={(on) => setForm({ ...form, path_style: on })}>Path-style URLs (MinIO)</CheckboxField>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="ghost" onClick={() => setOpen(false)}>Cancel</Button>
              <Button type="submit" disabled={save.isPending}>{save.isPending ? 'Saving…' : 'Save'}</Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
    </Page>
  )
}

// ---------------------------------------------------------------- Backups
export function BackupsSettings() {
  const [busy, setBusy] = useState(false)
  const [restoreRun, setRestoreRun] = useState<BackupRun | null>(null)
  const [snapFile, setSnapFile] = useState<File | null>(null)
  const [confirmText, setConfirmText] = useState('')
  const fileRef = useRef<HTMLInputElement>(null)
  const [cluster, setCluster] = useState<ClusterBackupSchedule>({
    enabled: false, target_id: '', cron: '0 4 * * *', retain: 14,
  })
  const vol = useQuery({
    queryKey: ['backups'],
    queryFn: fetchBackups,
    retry: false,
    enabled: !isMock,
  })
  useEffect(() => {
    if (vol.data?.cluster) setCluster({ ...vol.data.cluster })
  }, [vol.data])

  async function download() {
    setBusy(true)
    try {
      const chunks: BlobPart[] = []
      for await (const c of api.cluster.backup({})) chunks.push(c.data as BlobPart)
      const a = document.createElement('a')
      a.href = URL.createObjectURL(new Blob(chunks))
      a.download = `aetherix-backup-${new Date().toISOString().slice(0, 10)}.snapshot`
      a.click()
      URL.revokeObjectURL(a.href)
      toast.success('Snapshot downloaded')
    } catch (e) {
      toast.error(isUnimplemented(e) ? 'Backups are not available on this node' : errMessage(e))
    } finally {
      setBusy(false)
    }
  }

  async function uploadRestore() {
    if (!snapFile) return
    setBusy(true)
    try {
      const res = await restoreClusterSnapshot(snapFile, true)
      toast.success(`Restored ${res.rows} rows (${res.machines_removed} machines removed)`)
      setSnapFile(null)
      setConfirmText('')
    } catch (e) {
      toast.error(errMessage(e))
    } finally {
      setBusy(false)
    }
  }

  const save = useAction(() => saveClusterBackupSchedule(cluster), { invalidate: [['backups']], success: 'Cluster backup schedule saved' })
  const runVolumes = useAction(() => runBackupNow(), { invalidate: [['backups']], success: 'Volume backups started' })
  const runCluster = useAction(() => runClusterBackupNow(), { invalidate: [['backups']], success: 'Cluster snapshot uploaded' })
  const restore = useAction(
    () => restoreBackup({ run_id: restoreRun!.id, service_id: restoreRun!.service_id, volume: restoreRun!.volume, key: restoreRun!.key }),
    {
      invalidate: [['backups']],
      success: 'Volume restore started',
      onSuccess: () => setRestoreRun(null),
    },
  )

  const targets = vol.data?.targets ?? []
  const okVolumeRuns = (vol.data?.runs ?? []).filter((r) => r.status === 'ok' && r.key && (r.kind ?? 'volume') === 'volume')
  const confirmOk = confirmText.trim().toLowerCase() === 'destroy cluster state'

  return (
    <Page>
      <PageHeader
        title="Backups"
        hideTitle
        description="Manual cluster export/restore, scheduled cluster snapshots to S3, and volume backup history. Configure destinations under S3 Targets; per-volume schedule and retain live on each service volume."
      />
      <div className="space-y-4">
        <Card>
          <CardHeader>
            <CardTitle>Cluster snapshot</CardTitle>
            <CardDescription>
              Download or restore the replicated store (same as <code>ax cluster backup</code> / <code>ax cluster restore</code>).
              Volume data on disk is not included.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-wrap gap-2">
            <Button onClick={download} disabled={busy}><Download />{busy ? 'Exporting…' : 'Download snapshot'}</Button>
            <Button variant="secondary" disabled={busy} onClick={() => fileRef.current?.click()}><Upload />Upload &amp; restore…</Button>
            <input ref={fileRef} type="file" accept=".snapshot,application/json,.json" className="hidden" onChange={(e) => {
              const f = e.target.files?.[0]
              if (f) { setSnapFile(f); setConfirmText('') }
              e.target.value = ''
            }} />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Scheduled cluster backup</CardTitle>
            <CardDescription>
              Upload a full <code>.snapshot</code> (including identity secrets) to an S3 target on a cron.
              Manage destinations in{' '}
              <Link to="/settings/s3-targets" className="text-primary underline-offset-2 hover:underline">S3 Targets</Link>.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {vol.isError ? <p className="text-sm text-muted-foreground">Backup API unavailable on this node.</p> : (
              <form className="space-y-3" onSubmit={(e) => { e.preventDefault(); save.mutate(undefined) }}>
                <CheckboxField checked={cluster.enabled} onCheckedChange={(on) => setCluster({ ...cluster, enabled: on })}>Enabled</CheckboxField>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="S3 target">
                    <Select value={cluster.target_id} onChange={(e) => setCluster({ ...cluster, target_id: e.target.value })}>
                      <option value="">Default target</option>
                      {targets.map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
                    </Select>
                  </Field>
                  <Field label="Cron (UTC)" hint="5-field; default daily at 04:00">
                    <Input value={cluster.cron} onChange={(e) => setCluster({ ...cluster, cron: e.target.value })} />
                  </Field>
                  <Field label="Retain snapshots">
                    <Input type="number" min={1} value={cluster.retain} onChange={(e) => setCluster({ ...cluster, retain: Math.max(1, Number(e.target.value) || 14) })} />
                  </Field>
                </div>
                <div className="flex flex-wrap gap-2">
                  <Button type="submit" disabled={save.isPending}>Save</Button>
                  <Button type="button" variant="secondary" disabled={runCluster.isPending || !targets.length} onClick={() => runCluster.mutate(undefined)}>Upload now</Button>
                </div>
                {!targets.length && (
                  <p className="text-xs text-muted-foreground">
                    Add an S3 target first — <Link to="/settings/s3-targets" className="text-primary underline-offset-2 hover:underline">Settings → S3 Targets</Link>.
                  </p>
                )}
              </form>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Volume backups</CardTitle>
            <CardDescription>
              Each volume with policy <code>backup-restore</code> chooses its own S3 target, schedule, and retain count on the service.
              Recent runs appear below; restore extracts a snapshot into the Docker volume.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-wrap gap-2">
              <Button variant="secondary" disabled={runVolumes.isPending || !targets.length} onClick={() => runVolumes.mutate(undefined)}>
                Run all volume backups now
              </Button>
            </div>
            {!!vol.data?.runs?.length && (
              <div className="pt-1">
                <div className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">Recent runs</div>
                <ul className="space-y-1 text-sm">
                  {vol.data.runs.slice(0, 15).map((r) => (
                    <li key={r.id} className="flex items-center justify-between gap-2 border-b border-border/60 py-1.5">
                      <span className="truncate">
                        {(r.kind ?? 'volume') === 'cluster' ? 'cluster snapshot' : r.volume} · {r.status}{r.error ? `: ${r.error}` : ''}
                      </span>
                      <span className="flex shrink-0 items-center gap-2 text-muted-foreground">
                        {r.bytes ? formatBytes(r.bytes) : '—'} · {timeAgo(new Date(r.at))}
                        {(r.kind ?? 'volume') === 'volume' && r.status === 'ok' && r.key && (
                          <Button size="sm" variant="outline" className="h-7" onClick={() => setRestoreRun(r)}><RotateCcw className="size-3.5" />Restore</Button>
                        )}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            )}
            {!!okVolumeRuns.length && (
              <div className="flex justify-end pt-1">
                <Button size="sm" variant="secondary" onClick={() => setRestoreRun(okVolumeRuns[0])}><RotateCcw />Restore latest volume</Button>
              </div>
            )}
          </CardContent>
        </Card>
      </div>
      <Dialog open={!!restoreRun} onOpenChange={(o) => !o && setRestoreRun(null)}>
        <DialogContent>
          <DialogHeader
            title="Restore volume backup?"
            description="The service will be scaled to zero while the snapshot is extracted into the Docker volume, then scaled back to its previous replica count."
          />
          {restoreRun && (
            <div className="mb-4 space-y-1 text-sm">
              <div><span className="text-muted-foreground">Volume</span> <code>{restoreRun.volume}</code></div>
              <div><span className="text-muted-foreground">Service</span> <code>{restoreRun.service_id}</code></div>
              <div className="truncate"><span className="text-muted-foreground">Object</span> <code className="text-xs">{restoreRun.key}</code></div>
              <div><span className="text-muted-foreground">Taken</span> {timeAgo(new Date(restoreRun.at))}</div>
              <p className="rounded-md border border-amber-500/30 bg-amber-500/10 px-2.5 py-2 text-xs text-amber-950 dark:text-amber-100">
                Brief downtime: Aetherix stops this app before restore and starts it again afterward.
              </p>
            </div>
          )}
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setRestoreRun(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => restore.mutate(undefined)} disabled={restore.isPending}>{restore.isPending ? 'Stopping & restoring…' : 'Stop app & restore'}</Button>
          </div>
        </DialogContent>
      </Dialog>
      <Dialog open={!!snapFile} onOpenChange={(o) => { if (!o) { setSnapFile(null); setConfirmText('') } }}>
        <DialogContent>
          <DialogHeader
            title="Restore cluster snapshot?"
            description="This destroys current cluster state: store tables are replaced, peer machines and replicas are cleared. Prefer a freshly initialised single-node cluster. Local volume data is not restored."
          />
          {snapFile && (
            <div className="mb-4 space-y-3 text-sm">
              <div><span className="text-muted-foreground">File</span> <code>{snapFile.name}</code> ({formatBytes(snapFile.size)})</div>
              <Field label='Type “destroy cluster state” to confirm'>
                <Input value={confirmText} onChange={(e) => setConfirmText(e.target.value)} autoComplete="off" placeholder="destroy cluster state" />
              </Field>
            </div>
          )}
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => { setSnapFile(null); setConfirmText('') }}>Cancel</Button>
            <Button variant="destructive" disabled={!confirmOk || busy} onClick={() => void uploadRestore()}>
              {busy ? 'Restoring…' : 'Restore snapshot'}
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </Page>
  )
}

// ---------------------------------------------------------------- Updates
function normVer(v?: string) {
  const s = (v || '').trim().replace(/^V/, '')
  if (!s || s === '—' || s === 'dev') return s
  return s.startsWith('v') ? s : (/^\d+\./.test(s) ? `v${s}` : s)
}

export function UpdatesSettings() {
  const st = useStatus()
  const machines = useQuery({
    queryKey: keys.machines,
    queryFn: async () => (await api.machines.listMachines({})).machines,
    refetchInterval: 8000,
  })
  const upgrade = useQuery({
    queryKey: ['upgrade'],
    queryFn: () => api.cluster.getUpgrade({}),
    refetchInterval: 5000,
  })
  const [channel, setChannel] = useState('latest')
  const [version, setVersion] = useState('')
  const [mirror, setMirror] = useState('')
  const [selected, setSelected] = useState<string[]>([])

  const check = useAction(() => api.cluster.checkUpdates({ channel }), {
    success: (r) => {
      setVersion(r.latestVersion)
      return r.updateAvailable ? `Update available: ${r.latestVersion}` : `On latest (${r.latestVersion})`
    },
  })
  const start = useAction(
    () => api.cluster.startUpgrade({
      version,
      channel,
      // Empty = all behind; otherwise only selected nodes that are not already on target.
      machineIds: selected.length ? selected.filter((id) => {
        const row = rows.find((m) => m.machineId === id)
        return row && normVer(version) && normVer(row.agentVersion) !== normVer(version)
      }) : [],
      downloadBase: mirror.trim(),
    }),
    { invalidate: [['upgrade'], keys.machines, keys.status], success: 'Upgrade started — nodes will self-update' },
  )
  const cancel = useAction(() => api.cluster.cancelUpgrade({}), {
    invalidate: [['upgrade'], keys.machines],
    success: 'Upgrade cancelled',
  })

  const status = upgrade.data
  const target = normVer(version || status?.desiredVersion)
  const rows = status?.machines?.length
    ? status.machines
    : (machines.data ?? []).map((m) => ({
        machineId: m.id,
        machineName: m.name,
        agentVersion: m.agentVersion || '—',
        arch: m.arch,
        upgradeStatus: m.upgradeStatus,
        upgradeMessage: m.upgradeMessage,
        onTarget: false,
      }))

  const alreadyOnTarget = (agentVersion?: string) => !!target && normVer(agentVersion) === target
  const upgradable = rows.filter((m) => !alreadyOnTarget(m.agentVersion))
  const selectedUpgradable = selected.length
    ? selected.filter((id) => upgradable.some((m) => m.machineId === id))
    : upgradable.map((m) => m.machineId)
  const canUpgrade = !!normVer(version) && selectedUpgradable.length > 0

  const toggle = (id: string) => setSelected((s) => (s.includes(id) ? s.filter((x) => x !== id) : [...s, id]))

  return (
    <Page>
      <PageHeader
        title="Updates"
        hideTitle
        description="Each node shows its aetherixd version. Machines already on the target release cannot be upgraded again."
      />
      <div className="space-y-4">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2"><ArrowUpCircle className="size-4" />Release</CardTitle>
            <CardDescription>
              This API is serving {st.data?.version || '—'} from {st.data?.servedBy || '—'}.
              {status?.desiredVersion ? ` Desired: ${status.desiredVersion} (${status.onTarget}/${status.total} on target).` : ''}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-3 sm:grid-cols-3">
              <Field label="Channel">
                <Select value={channel} onChange={(e) => setChannel(e.target.value)}>
                  <option value="latest">latest</option>
                  <option value="stable">stable</option>
                </Select>
              </Field>
              <Field label="Target version" hint="e.g. v0.1.0">
                <Input value={version} onChange={(e) => setVersion(e.target.value)} placeholder="v0.1.0" />
              </Field>
              <Field label="Download mirror" hint="Optional; leave blank for GitHub Releases (Aetherix-code/AetherixCloud).">
                <Input value={mirror} onChange={(e) => setMirror(e.target.value)} placeholder="https://…" />
              </Field>
            </div>
            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="secondary" disabled={check.isPending} onClick={() => check.mutate(undefined)}>
                Check for updates
              </Button>
              <Button size="sm" disabled={!canUpgrade || start.isPending} onClick={() => start.mutate(undefined)}>
                Upgrade {selected.length ? `${selectedUpgradable.length} selected` : `${upgradable.length} behind`}
              </Button>
              {!!status?.desiredVersion && (
                <Button size="sm" variant="ghost" disabled={cancel.isPending} onClick={() => cancel.mutate(undefined)}>
                  Cancel target
                </Button>
              )}
            </div>
            {!!target && upgradable.length === 0 && rows.length > 0 && (
              <p className="text-xs text-muted-foreground">Every machine is already on {target}.</p>
            )}
            <p className="text-xs text-muted-foreground">
              Prefer downloading a cluster snapshot under Backups before a major upgrade. In local DinD (`AX_DEV`), binaries install then wait for a container restart.
            </p>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Machines</CardTitle>
            <CardDescription>Version per node from heartbeat. Leave selection empty to upgrade every node that is behind the target.</CardDescription>
          </CardHeader>
          <CardContent className="p-0">
            {upgrade.isLoading && machines.isLoading ? <div className="p-4"><ListSkeleton rows={3} /></div>
              : upgrade.error && machines.error ? <div className="p-4"><QueryError error={upgrade.error} what="upgrade status" /></div>
                : (
                  <div className="divide-y">
                    {rows.length === 0 && <p className="p-4 text-sm text-muted-foreground">No machines.</p>}
                    {rows.map((m) => {
                      const current = alreadyOnTarget(m.agentVersion)
                      const verLabel = m.agentVersion && m.agentVersion !== '—' ? normVer(m.agentVersion) : '—'
                      return (
                        <label key={m.machineId} className={`flex items-center gap-3 p-3 text-sm ${current ? 'opacity-60' : 'cursor-pointer hover:bg-muted/40'}`}>
                          <Checkbox
                            checked={selected.includes(m.machineId)}
                            disabled={current}
                            onCheckedChange={() => !current && toggle(m.machineId)}
                            aria-label={`Select ${m.machineName}`}
                          />
                          <div className="min-w-0 flex-1">
                            <div className="font-medium">{m.machineName}</div>
                            <div className="truncate text-xs text-muted-foreground">
                              <span className="font-mono">aetherixd {verLabel}</span>
                              {m.arch ? ` · ${m.arch}` : ''}
                              {m.upgradeMessage ? ` · ${m.upgradeMessage}` : ''}
                            </div>
                          </div>
                          {current ? <Badge tone="success">up to date</Badge>
                            : m.onTarget ? <Badge tone="success">on target</Badge>
                              : m.upgradeStatus ? <Badge tone={m.upgradeStatus === 'failed' ? 'danger' : 'warning'}>{m.upgradeStatus}</Badge>
                                : status?.desiredVersion ? <Badge tone="warning">behind</Badge>
                                  : <Badge>idle</Badge>}
                        </label>
                      )
                    })}
                  </div>
                )}
          </CardContent>
        </Card>
      </div>
    </Page>
  )
}

// ---------------------------------------------------------------- General
export function GeneralSettings() {
  const st = useStatus()
  const c = st.data?.cluster
  const [name, setName] = useState('')
  const [domain, setDomain] = useState('')
  useEffect(() => {
    if (c) {
      setName(c.name)
      setDomain(c.domain)
    }
  }, [c])
  const save = useAction(() => api.cluster.updateCluster({ name, domain }), { invalidate: [keys.status], success: 'Cluster updated' })
  return (
    <Page>
      <PageHeader title="General" hideTitle description="Cluster identity used in the sidebar, default hostnames and GitHub webhook URLs." />
      {st.isLoading ? <ListSkeleton rows={2} /> : st.error ? <QueryError error={st.error} what="cluster" /> : (
        <Card>
          <CardHeader>
            <CardTitle>Cluster identity</CardTitle>
            <CardDescription>Network {c?.network || '—'} · serving {st.data?.version || '—'} from {st.data?.servedBy || '—'}</CardDescription>
          </CardHeader>
          <CardContent>
            <form className="space-y-4" onSubmit={(e) => { e.preventDefault(); save.mutate(undefined) }}>
              <Field label="Cluster name"><Input value={name} onChange={(e) => setName(e.target.value)} /></Field>
              <Field label="Dashboard domain" hint="Used for default hostnames and the GitHub webhook. Leave empty to use &lt;ip&gt;.sslip.io."><Input value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="dashboard.example.com" /></Field>
              <Button type="submit" disabled={save.isPending}>Save changes</Button>
            </form>
          </CardContent>
        </Card>
      )}
    </Page>
  )
}
