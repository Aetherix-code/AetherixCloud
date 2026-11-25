import { describe, expect, it } from 'vitest'
import { emptyVolume, volumeFromSpec, volumeToSpec, volumeValid } from './volumes'

describe('volumes', () => {
  it('round-trips size MiB ↔ bytes', () => {
    const d = emptyVolume({ source: 'data', target: '/var/lib/data', sizeMiB: 1024 })
    const spec = volumeToSpec(d)
    expect(spec.sizeBytes).toBe(1024n * 1024n * 1024n)
    expect(volumeFromSpec(spec).sizeMiB).toBe(1024)
  })

  it('validates named volume + absolute mount', () => {
    expect(volumeValid(emptyVolume({ source: 'pgdata', target: '/var/lib/postgresql/data' }))).toBe(true)
    expect(volumeValid(emptyVolume({ source: 'bad name', target: '/data' }))).toBe(false)
    expect(volumeValid(emptyVolume({ source: 'data', target: 'relative' }))).toBe(false)
    expect(volumeValid(emptyVolume({ source: '/host/path', target: '/data' }))).toBe(true)
  })

  it('normalizes persistence', () => {
    expect(volumeFromSpec({ source: 'a', target: '/a', persistence: 'backup-restore' }).persistence).toBe('backup-restore')
    expect(volumeFromSpec({ source: 'a', target: '/a' }).persistence).toBe('local')
    expect(volumeToSpec(emptyVolume({ source: 'a', target: '/a', persistence: 'local' })).persistence).toBe('local')
  })
})
