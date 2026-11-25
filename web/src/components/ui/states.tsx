import { AlertTriangle, Inbox, PlugZap } from 'lucide-react'
import type { ReactNode } from 'react'
import { errMessage, isUnimplemented } from '@/lib/api'
import { Card } from './card'
import { Skeleton } from './skeleton'

export function Empty({ title, children, icon }: { title: string; children?: ReactNode; icon?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-2.5 px-6 py-14 text-center">
      <div className="grid size-12 place-items-center rounded-xl bg-muted/70 text-muted-foreground">{icon ?? <Inbox className="size-6" />}</div>
      <p className="text-sm font-semibold tracking-tight">{title}</p>
      {children && <div className="max-w-md text-sm leading-relaxed text-muted-foreground">{children}</div>}
    </div>
  )
}

/** Shown when an RPC isn't implemented by this backend build yet. */
export function NotAvailable({ what }: { what: string }) {
  return (
    <Card className="border-dashed">
      <Empty title={`${what} is not available on this node yet`} icon={<PlugZap className="size-8" />}>
        The backend answered <code className="rounded bg-muted px-1">Unimplemented</code>. The UI is wired to the proto contract and will light up when the service ships. Run with <code className="rounded bg-muted px-1">VITE_MOCK=1</code> to preview it.
      </Empty>
    </Card>
  )
}

export function QueryError({ error, what }: { error: unknown; what: string }) {
  if (isUnimplemented(error)) return <NotAvailable what={what} />
  return (
    <Card className="border-destructive/40">
      <Empty title={`Couldn't load ${what}`} icon={<AlertTriangle className="size-8 text-destructive" />}>
        {errMessage(error)}
      </Empty>
    </Card>
  )
}

export function ListSkeleton({ rows = 4 }: { rows?: number }) {
  return (
    <div className="space-y-2" aria-busy="true">
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-14 w-full" />
      ))}
    </div>
  )
}
