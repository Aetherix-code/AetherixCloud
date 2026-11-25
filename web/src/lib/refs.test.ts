import { describe, expect, it } from 'vitest'
import { parseEnvText, parseRefs, refEdges, resolveRefs, toEnvText } from './refs'

describe('variable references', () => {
  it('parses references', () => {
    expect(parseRefs('postgres://${{postgres.USER}}@${{ postgres.HOST }}/${{redis.URL}}')).toEqual([
      { service: 'postgres', key: 'USER' },
      { service: 'postgres', key: 'HOST' },
      { service: 'redis', key: 'URL' },
    ])
  })

  it('builds deduped edges for known services only', () => {
    const edges = refEdges(
      [
        { serviceName: 'api', value: '${{postgres.URL}}' },
        { serviceName: 'api', value: '${{postgres.HOST}}' },
        { serviceName: 'api', value: '${{ghost.X}}' },
        { serviceName: 'api', value: '${{api.SELF}}' },
        { serviceName: 'web', value: '${{api.URL}}' },
      ],
      new Set(['api', 'postgres', 'web']),
    )
    expect(edges.map((e) => `${e.from}->${e.to}`)).toEqual(['api->postgres', 'web->api'])
  })

  it('resolves references and leaves unknown intact', () => {
    const out = resolveRefs('a=${{x.K}} b=${{y.K}}', (s, k) => (s === 'x' ? `${s}-${k}` : undefined))
    expect(out).toBe('a=x-K b=${{y.K}}')
  })

  it('round-trips env text', () => {
    const parsed = parseEnvText('# c\nA=1\nB="two words"\n\nbad\nC=x=y')
    expect(parsed).toEqual([
      { key: 'A', value: '1' },
      { key: 'B', value: 'two words' },
      { key: 'C', value: 'x=y' },
    ])
    expect(toEnvText([{ key: 'A', value: '1' }, { key: 'S', value: 'x', redacted: true }])).toBe('A=1\nS=')
  })
})
