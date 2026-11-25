import { useSyncExternalStore } from 'react'
import { cn } from '@/lib/utils'

interface ToastItem {
  id: number
  message: string
  tone: 'default' | 'error' | 'success'
}
let items: ToastItem[] = []
let nextId = 1
const subs = new Set<() => void>()
const emitChange = () => subs.forEach((f) => f())

export function toast(message: string, tone: ToastItem['tone'] = 'default') {
  const id = nextId++
  items = [...items, { id, message, tone }]
  emitChange()
  setTimeout(() => {
    items = items.filter((t) => t.id !== id)
    emitChange()
  }, tone === 'error' ? 7000 : 3500)
}
toast.error = (m: string) => toast(m, 'error')
toast.success = (m: string) => toast(m, 'success')

export function Toaster() {
  const list = useSyncExternalStore(
    (cb) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    () => items,
  )
  return (
    <div className="pointer-events-none fixed bottom-4 right-4 z-[100] flex flex-col gap-2" role="status" aria-live="polite">
      {list.map((t) => (
        <div
          key={t.id}
          className={cn(
            'ax-enter pointer-events-auto max-w-sm rounded-lg border bg-card/95 px-4 py-2.5 text-sm ax-panel backdrop-blur',
            t.tone === 'error' && 'border-destructive/40 text-destructive',
            t.tone === 'success' && 'border-success/40 text-success',
          )}
        >
          {t.message}
        </div>
      ))}
    </div>
  )
}
