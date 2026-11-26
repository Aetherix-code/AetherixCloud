import type { Service } from '@/gen/aetherix/v1/types_pb'
import { fetchBuildPlan, type BuildPlanResponse } from '@/lib/extras'

export interface DetectedPlan {
  provider: 'dockerfile' | 'railpack' | 'image' | 'compose'
  language?: string
  version?: string
  install?: string
  build?: string
  start?: string
  source: 'configured' | 'heuristic' | 'railpack'
  reason?: string
}

/**
 * Client-side fallback when `/api/builds/plan` is unavailable. Prefers the
 * service BuildSpec; otherwise guesses from the GitHub repo name.
 */
export function detectPlan(s: Service): DetectedPlan {
  const spec = s.spec
  if (!spec?.source?.github) return { provider: 'image', source: 'configured' }
  const b = spec.build
  const explicit = b?.builder
  if (explicit === 'dockerfile' || (!explicit && b?.dockerfile)) {
    return { provider: 'dockerfile', source: 'configured', build: b?.dockerfile || 'Dockerfile' }
  }
  if (explicit === 'railpack' || b?.startCommand || b?.buildCommand) {
    return {
      provider: 'railpack',
      source: 'configured',
      build: b?.buildCommand || undefined,
      start: b?.startCommand || undefined,
    }
  }
  const repo = spec.source.github.repo.toLowerCase()
  const go = /(^|[-_/])(go|golang)([-_]|$)/.test(repo)
  const py = /(py|python|django|flask)/.test(repo)
  const lang = go ? 'Go' : py ? 'Python' : 'Node'
  return {
    provider: 'railpack',
    language: lang,
    version: go ? '1.23' : py ? '3.12' : '22',
    install: go ? 'go mod download' : py ? 'pip install -r requirements.txt' : 'pnpm install --frozen-lockfile',
    build: b?.buildCommand || (go ? 'go build -o out ./...' : py ? '' : 'pnpm run build'),
    start: b?.startCommand || (go ? './out' : py ? 'gunicorn app:app' : 'pnpm run start'),
    source: 'heuristic',
  }
}

/** Map a builder `/api/builds/plan` response into the dashboard card shape. */
export function planFromAPI(r: BuildPlanResponse, fallback: DetectedPlan): DetectedPlan {
  const provider = (r.plan?.provider || fallback.provider) as DetectedPlan['provider']
  const d = r.detected
  if (!d && provider !== 'railpack') {
    return {
      provider,
      source: 'configured',
      build: r.plan?.dockerfile || fallback.build,
      reason: r.plan?.reason,
    }
  }
  const lang = d?.language || d?.providers?.[0] || fallback.language
  const version = (d?.packages && lang && d.packages[lang]) || fallback.version
  return {
    provider: provider || 'railpack',
    language: lang,
    version,
    install: d?.install_commands?.join(' && ') || fallback.install,
    build: d?.build_commands?.join(' && ') || fallback.build,
    start: d?.start_command || fallback.start,
    source: 'railpack',
    reason: r.plan?.reason,
  }
}

/** Live plan from a builder node; falls back to {@link detectPlan} on error. */
export async function loadBuildPlan(service: Service): Promise<DetectedPlan> {
  const fallback = detectPlan(service)
  if (!service.id || !service.spec?.source?.github) return fallback
  try {
    const r = await fetchBuildPlan(service.id)
    return planFromAPI(r, fallback)
  } catch {
    return fallback
  }
}
