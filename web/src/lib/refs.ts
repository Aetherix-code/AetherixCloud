// Variable references: ${{service.KEY}} resolve to another service's variable.
// They also define the edges on the project canvas.
const REF = /\$\{\{\s*([A-Za-z0-9_.-]+)\.([A-Za-z0-9_]+)\s*\}\}/g

export interface VarRef {
  service: string
  key: string
}

export function parseRefs(value: string): VarRef[] {
  const out: VarRef[] = []
  for (const m of value.matchAll(REF)) out.push({ service: m[1], key: m[2] })
  return out
}

export interface RefEdge {
  from: string // service that holds the variable
  to: string // referenced service
  key: string
}

/** Edges service -> referenced service, deduped, ignoring self-refs and unknown services. */
export function refEdges(vars: { serviceName: string; value: string }[], known: Set<string>): RefEdge[] {
  const seen = new Set<string>()
  const edges: RefEdge[] = []
  for (const v of vars) {
    for (const r of parseRefs(v.value)) {
      if (r.service === v.serviceName || !known.has(r.service)) continue
      const id = `${v.serviceName}->${r.service}`
      if (seen.has(id)) continue
      seen.add(id)
      edges.push({ from: v.serviceName, to: r.service, key: r.key })
    }
  }
  return edges
}

/** Resolve references against a lookup; unresolved refs are left intact. */
export function resolveRefs(value: string, lookup: (svc: string, key: string) => string | undefined): string {
  return value.replace(REF, (whole, svc: string, key: string) => lookup(svc, key) ?? whole)
}

/** Parse a raw `KEY=value` editor buffer. Comments and blanks are ignored. */
export function parseEnvText(text: string): { key: string; value: string }[] {
  const out: { key: string; value: string }[] = []
  for (const raw of text.split('\n')) {
    const line = raw.trim()
    if (!line || line.startsWith('#')) continue
    const i = line.indexOf('=')
    if (i <= 0) continue
    let value = line.slice(i + 1).trim()
    if (/^(".*"|'.*')$/.test(value)) value = value.slice(1, -1)
    out.push({ key: line.slice(0, i).trim(), value })
  }
  return out
}

export function toEnvText(vars: { key: string; value: string; redacted?: boolean }[]): string {
  return vars.map((v) => `${v.key}=${v.redacted ? '' : v.value}`).join('\n')
}
