import type { RefEdge } from './refs'

export interface Pos {
  x: number
  y: number
}

/**
 * Layered layout for the project canvas: a service sits to the left of the
 * services it references (web -> api -> postgres). Cycles are tolerated.
 */
export function layoutServices(names: string[], edges: RefEdge[], dx = 320, dy = 170): Record<string, Pos> {
  const depth: Record<string, number> = Object.fromEntries(names.map((n) => [n, 0]))
  for (let i = 0; i < names.length; i++) {
    let changed = false
    for (const e of edges) {
      const d = depth[e.from] + 1
      if (d > depth[e.to] && d < names.length) {
        depth[e.to] = d
        changed = true
      }
    }
    if (!changed) break
  }
  const rows: Record<number, number> = {}
  const out: Record<string, Pos> = {}
  for (const n of names) {
    const d = depth[n]
    const r = rows[d] ?? 0
    rows[d] = r + 1
    out[n] = { x: d * dx, y: r * dy }
  }
  return out
}
