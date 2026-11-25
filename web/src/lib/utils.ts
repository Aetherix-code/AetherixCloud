import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'
import type { Timestamp } from '@bufbuild/protobuf/wkt'
import { timestampDate } from '@bufbuild/protobuf/wkt'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function tsDate(t?: Timestamp): Date | undefined {
  return t ? timestampDate(t) : undefined
}

export function timeAgo(t?: Timestamp | Date, now = Date.now()): string {
  if (!t) return '—'
  const d = t instanceof Date ? t : timestampDate(t)
  const s = Math.max(0, Math.round((now - d.getTime()) / 1000))
  if (s < 5) return 'just now'
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export function formatBytes(n: bigint | number): string {
  let v = Number(n)
  if (!v) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

export function formatCpu(millis: bigint | number): string {
  const v = Number(millis)
  return v >= 1000 ? `${(v / 1000).toFixed(v % 1000 ? 1 : 0)} vCPU` : `${v}m`
}

export function pct(used: bigint | number, total: bigint | number): number {
  const t = Number(total)
  return t ? Math.min(100, Math.round((Number(used) / t) * 100)) : 0
}

export function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}

export function copy(text: string) {
  return navigator.clipboard?.writeText(text)
}
