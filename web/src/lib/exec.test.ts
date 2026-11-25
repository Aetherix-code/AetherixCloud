import { describe, expect, it } from 'vitest'
import { EnvelopeParser, encodeEnvelope } from './exec'

describe('connect envelopes', () => {
  it('encodes and decodes across arbitrary chunk boundaries', () => {
    const a = encodeEnvelope(new TextEncoder().encode('{"stdout":"aGk="}'))
    const b = encodeEnvelope(new TextEncoder().encode('{}'), 0x02)
    const all = new Uint8Array([...a, ...b])
    const p = new EnvelopeParser()
    const out = [...p.push(all.slice(0, 3)), ...p.push(all.slice(3, 20)), ...p.push(all.slice(20))]
    expect(out).toHaveLength(2)
    expect(new TextDecoder().decode(out[0].data)).toBe('{"stdout":"aGk="}')
    expect(out[1].flags).toBe(0x02)
  })
})
