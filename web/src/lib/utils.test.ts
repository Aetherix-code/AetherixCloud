import { describe, expect, it } from 'vitest'
import { formatBytes, formatCpu, pct, timeAgo } from './utils'

describe('format helpers', () => {
  it('formats bytes', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(8n * 1024n ** 3n)).toBe('8.0 GiB')
  })
  it('formats cpu and percentages', () => {
    expect(formatCpu(500)).toBe('500m')
    expect(formatCpu(2000)).toBe('2 vCPU')
    expect(pct(50, 200)).toBe(25)
    expect(pct(1, 0)).toBe(0)
  })
  it('formats relative time', () => {
    const now = Date.now()
    expect(timeAgo(new Date(now - 120_000), now)).toBe('2m ago')
    expect(timeAgo(undefined)).toBe('—')
  })
})
