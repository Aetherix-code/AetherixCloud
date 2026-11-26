// Host shell over /api/host-exec/ws (WebSocket). Admin-only; proxied over the mesh.
import { isMock, tokenStore } from './api'
import type { ExecHandlers, ExecSession } from './exec'

export interface HostExecTarget {
  machineId: string
  rows: number
  cols: number
}

const enc = new TextEncoder()

function b64encode(bytes: Uint8Array): string {
  let s = ''
  for (const b of bytes) s += String.fromCharCode(b)
  return btoa(s)
}

function b64decode(s: string): Uint8Array {
  const bin = atob(s)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

function wsURL(): string {
  const base = (import.meta.env.VITE_API_BASE as string | undefined) || window.location.origin
  const u = new URL('/api/host-exec/ws', base)
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:'
  const tok = tokenStore.get()
  if (tok) u.searchParams.set('token', tok)
  return u.toString()
}

export function openHostExec(target: HostExecTarget, h: ExecHandlers): ExecSession {
  if (isMock) return mockHostShell(h)

  let ws: WebSocket | null = null
  let closed = false
  const queue: string[] = []

  const send = (obj: unknown) => {
    const raw = JSON.stringify(obj)
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(raw)
    else queue.push(raw)
  }

  try {
    ws = new WebSocket(wsURL())
  } catch (e) {
    queueMicrotask(() => h.onError(e instanceof Error ? e.message : String(e)))
    return { write() {}, resize() {}, close() {} }
  }

  ws.onopen = () => {
    send({
      start: {
        machineId: target.machineId,
        rows: target.rows,
        cols: target.cols,
      },
    })
    for (const m of queue.splice(0)) ws?.send(m)
  }
  ws.onmessage = (ev) => {
    try {
      const msg = JSON.parse(String(ev.data)) as {
        stdout?: string
        stderr?: string
        exitCode?: number
        error?: string
      }
      if (msg.error) h.onError(msg.error)
      if (msg.stdout) h.onData(b64decode(msg.stdout))
      if (msg.stderr) h.onData(b64decode(msg.stderr))
      if (msg.exitCode !== undefined && msg.exitCode !== null) h.onExit(msg.exitCode)
    } catch (e) {
      if (!closed) h.onError(e instanceof Error ? e.message : String(e))
    }
  }
  ws.onerror = () => {
    if (!closed) h.onError('host shell connection failed')
  }

  return {
    write: (data) => send({ stdin: b64encode(enc.encode(data)) }),
    resize: (rows, cols) => send({ resize: { rows, cols } }),
    close: () => {
      closed = true
      try {
        ws?.close()
      } catch {
        /* already closed */
      }
    },
  }
}

function mockHostShell(h: ExecHandlers): ExecSession {
  const prompt = '\x1b[33mroot@node\x1b[0m:\x1b[34m~\x1b[0m# '
  let line = ''
  const out = (s: string) => h.onData(enc.encode(s))
  queueMicrotask(() => out(`\x1b[2mMock host shell (VITE_MOCK=1)\x1b[0m\r\n${prompt}`))
  return {
    write(data) {
      for (const ch of data) {
        if (ch === '\r') {
          const cmd = line.trim()
          line = ''
          out('\r\n')
          if (cmd === 'exit') return h.onExit(0)
          if (cmd) out(`mock: ${cmd}\r\n`)
          out(prompt)
        } else if (ch === '\x7f') {
          if (line) {
            line = line.slice(0, -1)
            out('\b \b')
          }
        } else if (ch >= ' ') {
          line += ch
          out(ch)
        }
      }
    },
    resize() {},
    close() {},
  }
}
