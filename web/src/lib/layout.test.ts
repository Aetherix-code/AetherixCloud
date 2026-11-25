import { describe, expect, it } from 'vitest'
import { layoutServices } from './layout'

describe('layoutServices', () => {
  it('places referenced services to the right', () => {
    const pos = layoutServices(['web', 'api', 'pg', 'solo'], [
      { from: 'web', to: 'api', key: 'URL' },
      { from: 'api', to: 'pg', key: 'DB' },
    ])
    expect(pos.web.x).toBeLessThan(pos.api.x)
    expect(pos.api.x).toBeLessThan(pos.pg.x)
    expect(pos.solo.x).toBe(pos.web.x)
    expect(pos.solo.y).toBeGreaterThan(pos.web.y)
  })
  it('terminates on cycles', () => {
    const pos = layoutServices(['a', 'b'], [{ from: 'a', to: 'b', key: 'k' }, { from: 'b', to: 'a', key: 'k' }])
    expect(Object.keys(pos)).toHaveLength(2)
  })
})
