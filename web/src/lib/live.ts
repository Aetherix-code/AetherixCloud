import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import type { Event } from '@/gen/aetherix/v1/types_pb'
import { api } from './api'
import { isStreamEvent, keys } from './queries'

/**
 * Subscribes to ClusterService.WatchEvents and refreshes the matching queries
 * so every view updates live. Reconnects with backoff; falls back to polling
 * (refetchInterval on the queries) when the stream can't be established.
 */
export function useLiveEvents(enabled: boolean): { live: boolean; recent: Event[] } {
  const qc = useQueryClient()
  const [live, setLive] = useState(false)
  const [recent, setRecent] = useState<Event[]>([])
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)

  useEffect(() => {
    if (!enabled) return
    const ctrl = new AbortController()
    let attempt = 0
    const invalidate = (e: Event) => {
      clearTimeout(timer.current)
      timer.current = setTimeout(() => {
        const p = e.type.split('.')[0]
        if (p === 'machine') void qc.invalidateQueries({ queryKey: keys.machines })
        if (p === 'deployment') void qc.invalidateQueries({ queryKey: ['deployments'] })
        void qc.invalidateQueries({ queryKey: ['service'] })
        void qc.invalidateQueries({ queryKey: ['project'] })
        void qc.invalidateQueries({ queryKey: keys.status })
        void qc.invalidateQueries({ queryKey: keys.events })
      }, 250)
    }
    ;(async () => {
      while (!ctrl.signal.aborted) {
        try {
          for await (const e of api.cluster.watchEvents({ replay: 0 }, { signal: ctrl.signal })) {
            setLive(true)
            attempt = 0
            if (isStreamEvent(e.type)) continue
            setRecent((r) => [e, ...r].slice(0, 50))
            invalidate(e)
          }
        } catch {
          /* fallthrough to retry */
        }
        setLive(false)
        if (ctrl.signal.aborted) return
        attempt++
        await new Promise((r) => setTimeout(r, Math.min(30000, 1000 * 2 ** attempt)))
      }
    })()
    return () => {
      ctrl.abort()
      clearTimeout(timer.current)
    }
  }, [enabled, qc])

  return { live, recent }
}
