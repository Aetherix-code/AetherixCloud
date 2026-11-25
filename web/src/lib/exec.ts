// Terminal exec over /api/exec/ws (WebSocket).
//
// Browsers cannot upload a ReadableStream body on HTTP/1.1 (Vite proxy) or in
// Safari, so the Connect bidi Exec RPC is only used by the CLI. The dashboard
// speaks a small JSON protocol over WebSocket instead.
import { isMock, tokenStore } from './api'

export interface ExecSession {
  write(data: string): void
  resize(rows: number, cols: number): void
  close(): void
}

export interface ExecHandlers {
  onData(data: Uint8Array): void
  onExit(code: number): void
  onError(message: string): void
}

export interface ExecTarget {
  serviceId: string
  replicaId?: string
  command?: string[]
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
  const u = new URL('/api/exec/ws', base)
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:'
  const tok = tokenStore.get()
  if (tok) u.searchParams.set('token', tok)
  return u.toString()
}

export function openExec(target: ExecTarget, h: ExecHandlers): ExecSession {
  if (isMock) return mockShell(h)

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
        serviceId: target.serviceId,
        replicaId: target.replicaId ?? '',
        command: target.command ?? ['/bin/sh'],
        tty: true,
        stdin: true,
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
    if (!closed) h.onError('terminal connection failed')
  }
  ws.onclose = () => {
    /* exitCode / error handlers already fired when applicable */
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

/** Kept for unit tests that still exercise Connect envelope framing. */
export function encodeEnvelope(payload: Uint8Array, flags = 0): Uint8Array {
  const out = new Uint8Array(5 + payload.length)
  out[0] = flags
  new DataView(out.buffer).setUint32(1, payload.length)
  out.set(payload, 5)
  return out
}

/** Incremental envelope decoder for a byte stream. */
export class EnvelopeParser {
  private buf = new Uint8Array(0)
  push(chunk: Uint8Array): { flags: number; data: Uint8Array }[] {
    const merged = new Uint8Array(this.buf.length + chunk.length)
    merged.set(this.buf)
    merged.set(chunk, this.buf.length)
    this.buf = merged
    const out: { flags: number; data: Uint8Array }[] = []
    while (this.buf.length >= 5) {
      const len = new DataView(this.buf.buffer, this.buf.byteOffset).getUint32(1)
      if (this.buf.length < 5 + len) break
      out.push({ flags: this.buf[0], data: this.buf.slice(5, 5 + len) })
      this.buf = this.buf.slice(5 + len)
    }
    return out
  }
}

/** Tiny fake shell for mock mode. */
function mockShell(h: ExecHandlers): ExecSession {
  const prompt = '\x1b[32mroot@replica\x1b[0m:\x1b[34m/app\x1b[0m# '
  let line = ''
  const out = (s: string) => h.onData(enc.encode(s))
  queueMicrotask(() => out(`\x1b[2mMock terminal (VITE_MOCK=1)\x1b[0m\r\n${prompt}`))
  return {
    write(data) {
      for (const ch of data) {
        if (ch === '\r') {
          const cmd = line.trim()
          line = ''
          out('\r\n')
          if (cmd === 'exit') return h.onExit(0)
          if (cmd) out(cmd === 'ls' ? 'Dockerfile  package.json  src  node_modules\r\n' : cmd.startsWith('echo ') ? `${cmd.slice(5)}\r\n` : `sh: ${cmd.split(' ')[0]}: mock shell\r\n`)
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
