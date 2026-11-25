import { useEffect, useRef } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { useTheme } from '@/lib/theme'
import { openExec, type ExecSession } from '@/lib/exec'

const darkTheme = { background: '#0a0b0f', foreground: '#d8dae3', cursor: '#8b5cf6', selectionBackground: '#3b2f6b' }
const lightTheme = { background: '#ffffff', foreground: '#1b1d24', cursor: '#7c3aed', selectionBackground: '#ddd6fe' }

function useXterm(opts: { cursorBlink?: boolean; disableStdin?: boolean }) {
  const host = useRef<HTMLDivElement>(null)
  const term = useRef<Terminal>(null)
  const fit = useRef<FitAddon>(null)
  const [theme] = useTheme()

  useEffect(() => {
    const t = new Terminal({
      fontFamily: 'ui-monospace, Menlo, monospace',
      fontSize: 12,
      cursorBlink: opts.cursorBlink,
      disableStdin: opts.disableStdin,
      convertEol: opts.disableStdin,
      scrollback: 5000,
      theme: document.documentElement.classList.contains('dark') ? darkTheme : lightTheme,
    })
    const f = new FitAddon()
    t.loadAddon(f)
    t.open(host.current!)
    term.current = t
    fit.current = f
    const doFit = () => {
      try {
        f.fit()
      } catch {
        /* host not laid out yet */
      }
    }
    doFit()
    const ro = new ResizeObserver(doFit)
    ro.observe(host.current!)
    return () => {
      ro.disconnect()
      t.dispose()
      term.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    if (term.current) term.current.options.theme = theme === 'dark' ? darkTheme : lightTheme
  }, [theme])

  return { host, term, fit }
}

export type LogEmit = (line: string, stream?: string) => void

/** Read-only log pane. `open` streams lines until aborted; it restarts when `deps` change. */
export function LogView({ open, deps, className }: { open: (emit: LogEmit, signal: AbortSignal) => Promise<void>; deps: unknown[]; className?: string }) {
  const { host, term } = useXterm({ disableStdin: true })
  useEffect(() => {
    const t = term.current
    if (!t) return
    t.reset()
    const ctrl = new AbortController()
    const emit: LogEmit = (line, stream) => t.writeln(stream === 'stderr' ? `\x1b[33m${line}\x1b[0m` : line)
    open(emit, ctrl.signal).catch((e) => {
      if (!ctrl.signal.aborted) t.writeln(`\x1b[31m${e instanceof Error ? e.message : String(e)}\x1b[0m`)
    })
    return () => ctrl.abort()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
  return <div ref={host} className={className ?? 'h-72 w-full overflow-hidden rounded-md border p-2'} data-testid="log-view" />
}

/** Interactive terminal attached to a replica via Exec. */
export function ExecTerminal({ serviceId, replicaId, className }: { serviceId: string; replicaId?: string; className?: string }) {
  const { host, term, fit } = useXterm({ cursorBlink: true })
  useEffect(() => {
    const t = term.current
    if (!t) return
    t.reset()
    const dec = new TextDecoder()
    const session: ExecSession = openExec(
      { serviceId, replicaId, rows: t.rows, cols: t.cols },
      {
        onData: (d) => t.write(dec.decode(d, { stream: true })),
        onExit: (c) => t.writeln(`\r\n\x1b[2m[process exited with code ${c}]\x1b[0m`),
        onError: (m) => t.writeln(`\r\n\x1b[31m[${m}]\x1b[0m`),
      },
    )
    const d1 = t.onData((d) => session.write(d))
    const d2 = t.onResize(({ rows, cols }) => session.resize(rows, cols))
    fit.current?.fit()
    t.focus()
    return () => {
      d1.dispose()
      d2.dispose()
      session.close()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [serviceId, replicaId])
  return <div ref={host} className={className ?? 'h-80 w-full overflow-hidden rounded-md border p-2'} data-testid="exec-terminal" />
}
