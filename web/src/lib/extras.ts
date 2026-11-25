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

export type BackupConfig = {
  enabled: boolean
  endpoint: string
  region: string
  bucket: string
  access_key_id: string
  secret_access_key: string
  prefix: string
  path_style: boolean
  cron: string
  service_ids?: string[]
}

export type BackupRun = {
  id: string
  service_id: string
  volume: string
  key: string
  bytes: number
  status: string
  error?: string
  at: string
}

export async function fetchBackups(): Promise<{ config: BackupConfig; runs: BackupRun[] }> {
  return getJSON('/api/backups')
}

export async function saveBackups(config: Partial<BackupConfig>): Promise<void> {
  await sendJSON('/api/backups', 'PUT', config)
}

export async function runBackupNow(): Promise<void> {
  await sendJSON('/api/backups/run', 'POST', {})
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
