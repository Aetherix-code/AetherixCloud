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

  it('round-trips backup schedule fields for backup-restore volumes', () => {
    const d = emptyVolume({
      source: 'pg', target: '/data', persistence: 'backup-restore',
      backupCron: '15 2 * * *', backupRetain: 3, s3TargetId: 'tgt1',
    })
    const spec = volumeToSpec(d)
    expect(spec.backupCron).toBe('15 2 * * *')
    expect(spec.backupRetain).toBe(3)
    expect(spec.s3TargetId).toBe('tgt1')
    const back = volumeFromSpec(spec)
    expect(back.backupCron).toBe('15 2 * * *')
    expect(back.backupRetain).toBe(3)
    expect(back.s3TargetId).toBe('tgt1')
  })

  it('clears backup fields when persistence is local', () => {
    const spec = volumeToSpec(emptyVolume({
      source: 'a', target: '/a', persistence: 'local',
      backupCron: '0 1 * * *', backupRetain: 9, s3TargetId: 'x',
    }))
    expect(spec.backupCron).toBe('')
    expect(spec.backupRetain).toBe(0)
    expect(spec.s3TargetId).toBe('')
  })
})
