import { describe, expect, it } from 'vitest'
import { detectPlan, planFromAPI } from './railpack'
import type { Service } from '@/gen/aetherix/v1/types_pb'

describe('railpack plan helpers', () => {
  it('falls back to railpack for a GitHub repo without Dockerfile settings', () => {
    const s = {
      spec: { source: { github: { repo: 'acme/my-node-app' } }, build: {} },
    } as Service
    const p = detectPlan(s)
    expect(p.provider).toBe('railpack')
    expect(p.source).toBe('heuristic')
    expect(p.language).toBe('Node')
  })

  it('maps a live /api/builds/plan response', () => {
    const fallback = detectPlan({
      spec: { source: { github: { repo: 'acme/web' } }, build: {} },
    } as Service)
    const live = planFromAPI(
      {
        plan: { provider: 'railpack', reason: 'no Dockerfile; using Railpack' },
        detected: {
          providers: ['node'],
          language: 'node',
          packages: { node: '22.1.0' },
          install_commands: ['npm ci'],
          build_commands: ['npm run build'],
          start_command: 'node server.js',
        },
      },
      fallback,
    )
    expect(live.source).toBe('railpack')
    expect(live.language).toBe('node')
    expect(live.version).toBe('22.1.0')
    expect(live.install).toBe('npm ci')
    expect(live.start).toBe('node server.js')
    expect(live.reason).toContain('Railpack')
  })
})
