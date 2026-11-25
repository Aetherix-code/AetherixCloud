import type { Service } from '@/gen/aetherix/v1/types_pb'

export interface DetectedPlan {
  provider: 'dockerfile' | 'railpack' | 'image' | 'compose'
  language?: string
  version?: string
  install?: string
  build?: string
  start?: string
  source: 'configured' | 'heuristic'
}

/**
 * Client-side preview of the build plan. The backend doesn't expose
 * `railpack prepare` output over RPC yet, so this derives what it can from the
 * service spec and repository name; the real plan replaces it on first build.
 */
export function detectPlan(s: Service): DetectedPlan {
  const spec = s.spec
  if (!spec?.source?.github) return { provider: 'image', source: 'configured' }
  const b = spec.build
  const explicit = b?.builder
  if (explicit === 'dockerfile' || (!explicit && b?.dockerfile)) return { provider: 'dockerfile', source: 'configured', build: b?.dockerfile || 'Dockerfile' }
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
