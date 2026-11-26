import { apiBase, isMock, tokenStore } from './api'

function headers(): HeadersInit {
  const t = tokenStore.get()
  return t ? { Authorization: `Bearer ${t}` } : {}
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${apiBase || window.location.origin}${path}`, { headers: headers() })
  if (!res.ok) throw new Error(await res.text() || res.statusText)
  return res.json() as Promise<T>
}

async function sendJSON<T>(path: string, method: string, body?: unknown): Promise<T> {
  const res = await fetch(`${apiBase || window.location.origin}${path}`, {
    method,
    headers: { ...headers(), 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) throw new Error(await res.text() || res.statusText)
  return res.json() as Promise<T>
}

export type MetricsAggregate = {
  cpu_percent: number[]
  memory_usage: number[]
  net_rx_bytes: number[]
  net_tx_bytes: number[]
  times: number[]
}

export type MetricsResponse = {
  service_id?: string
  series: unknown[]
  aggregate: MetricsAggregate
}

export async function fetchMetrics(serviceId: string): Promise<MetricsResponse | null> {
  if (isMock) return null
  try {
    return await getJSON<MetricsResponse>(`/api/metrics?service_id=${encodeURIComponent(serviceId)}`)
  } catch {
    return null
  }
}

/** Response from GET /api/builds/plan?service=… (Railpack / Dockerfile preview). */
export type BuildPlanResponse = {
  commit?: string
  plan?: {
    provider?: string
    reason?: string
    dockerfile?: string
    root_dir?: string
  }
  detected?: {
    providers?: string[]
    language?: string
    packages?: Record<string, string>
    install_commands?: string[]
    build_commands?: string[]
    start_command?: string
  }
}

export async function fetchBuildPlan(serviceId: string): Promise<BuildPlanResponse> {
  return getJSON(`/api/builds/plan?service=${encodeURIComponent(serviceId)}`)
}

export type S3Target = {
  id: string
  name: string
  endpoint: string
  region: string
  bucket: string
  access_key_id: string
  secret_access_key: string
  prefix: string
  path_style: boolean
  created_at?: string
}

export type ClusterBackupSchedule = {
  enabled: boolean
  target_id: string
  cron: string
  retain: number
}

export type BackupRun = {
  id: string
  kind?: string
  service_id: string
  volume: string
  key: string
  bytes: number
  status: string
  error?: string
  at: string
}

export type BuildImageArchiveConfig = {
  enabled: boolean
  target_id: string
  retain: number
}

export type BuildConcurrencyConfig = {
  max_parallel: number
}

export async function fetchBuildImageArchive(): Promise<BuildImageArchiveConfig> {
  return getJSON('/api/build-images')
}

export async function saveBuildImageArchive(cfg: Partial<BuildImageArchiveConfig>): Promise<void> {
  await sendJSON('/api/build-images', 'PUT', cfg)
}

export async function fetchBuildConcurrency(): Promise<BuildConcurrencyConfig> {
  return getJSON('/api/build-concurrency')
}

export async function saveBuildConcurrency(cfg: Partial<BuildConcurrencyConfig>): Promise<void> {
  await sendJSON('/api/build-concurrency', 'PUT', cfg)
}

export async function fetchS3Targets(): Promise<{ targets: S3Target[] }> {
  return getJSON('/api/s3-targets')
}

export async function createS3Target(body: Partial<S3Target>): Promise<S3Target> {
  return sendJSON('/api/s3-targets', 'POST', body)
}

export async function updateS3Target(id: string, body: Partial<S3Target>): Promise<S3Target> {
  return sendJSON(`/api/s3-targets/${encodeURIComponent(id)}`, 'PUT', body)
}

export async function deleteS3Target(id: string): Promise<void> {
  await sendJSON(`/api/s3-targets/${encodeURIComponent(id)}`, 'DELETE')
}

export async function fetchBackups(): Promise<{
  cluster: ClusterBackupSchedule
  runs: BackupRun[]
  targets: S3Target[]
}> {
  return getJSON('/api/backups')
}

export async function saveClusterBackupSchedule(cluster: Partial<ClusterBackupSchedule>): Promise<void> {
  await sendJSON('/api/backups', 'PUT', { cluster })
}

export async function runBackupNow(body?: { service_id?: string; volume?: string }): Promise<void> {
  await sendJSON('/api/backups/run', 'POST', body ?? {})
}

export async function runClusterBackupNow(): Promise<void> {
  await sendJSON('/api/backups/cluster/run', 'POST', {})
}

export async function restoreBackup(body: { run_id?: string; service_id?: string; volume?: string; key?: string }): Promise<{ ok: boolean; restored?: boolean; queued?: boolean }> {
  return sendJSON('/api/backups/restore', 'POST', body)
}

export type ClusterRestoreResult = {
  tables: number
  rows: number
  machines_removed: number
  replicas_cleared: number
}

/** Upload a .snapshot via plain POST (Connect client-streaming fails in browsers). */
export async function restoreClusterSnapshot(file: Blob, force: boolean): Promise<ClusterRestoreResult> {
  const url = `${apiBase || window.location.origin}/api/cluster/restore${force ? '?force=1' : ''}`
  const res = await fetch(url, {
    method: 'POST',
    headers: headers(),
    body: file,
  })
  if (!res.ok) throw new Error((await res.text()) || res.statusText)
  return res.json() as Promise<ClusterRestoreResult>
}
