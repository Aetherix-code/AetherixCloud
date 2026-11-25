import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Activity, Boxes, Hammer, Rocket, Server } from 'lucide-react'
import { Page, PageHeader } from '@/components/layout'
import { Card } from '@/components/ui/card'
import { Select } from '@/components/ui/input'
import { Empty, ListSkeleton, QueryError } from '@/components/ui/states'
import type { Event } from '@/gen/aetherix/v1/types_pb'
import { useEvents } from '@/lib/queries'
import { timeAgo, tsDate } from '@/lib/utils'

const icon = (t: string) => (t.startsWith('machine') ? <Server className="size-4" /> : t.startsWith('deployment') ? <Rocket className="size-4" /> : t.startsWith('build') ? <Hammer className="size-4" /> : t.startsWith('replica') || t.startsWith('service') ? <Boxes className="size-4" /> : <Activity className="size-4" />)

export function groupByDay(events: Event[]): [string, Event[]][] {
  const out = new Map<string, Event[]>()
  for (const e of events) {
    const d = tsDate(e.time)
    const k = d ? d.toLocaleDateString(undefined, { weekday: 'long', month: 'short', day: 'numeric' }) : 'Unknown'
    out.set(k, [...(out.get(k) ?? []), e])
  }
  return [...out]
}

export function ActivityPage() {
  const q = useEvents()
  const [type, setType] = useState('')
  const filtered = useMemo(() => (q.data ?? []).filter((e) => !type || e.type.startsWith(type)), [q.data, type])
  return (
    <Page>
      <PageHeader
        title="Activity"
        description="Operational cluster events — deployments, builds, machine state and replica changes. Updated live. Privileged mutations belong in the Audit log."
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <Select className="w-auto" value={type} onChange={(e) => setType(e.target.value)} aria-label="Filter by type">
              <option value="">All events</option>
              <option value="deployment">Deployments</option>
              <option value="build">Builds</option>
              <option value="machine">Machines</option>
              <option value="replica">Replicas</option>
              <option value="service">Services</option>
            </Select>
            <Link to="/settings/audit" className="text-sm text-muted-foreground underline-offset-2 hover:text-foreground hover:underline">
              Audit log →
            </Link>
          </div>
        }
      />
      {q.isLoading ? <ListSkeleton /> : q.error ? <QueryError error={q.error} what="events" /> : !filtered.length ? (
        <Card>
          <Empty title="No activity yet">
            Deploy a service or join a machine — events will stream in here.
          </Empty>
        </Card>
      ) : (
        <div className="space-y-5">
          {groupByDay(filtered).map(([day, evs]) => (
            <section key={day}>
              <h2 className="mb-2 text-[11px] font-semibold uppercase tracking-[0.07em] text-muted-foreground">{day}</h2>
              <Card className="divide-y">
                {evs.map((e) => (
                  <div key={e.id} className="ax-row flex items-center gap-3 px-4 py-2.5 text-sm hover:bg-muted/40">
                    <span className="text-muted-foreground">{icon(e.type)}</span>
                    <span className="min-w-0 flex-1 truncate">{e.message}</span>
                    <code className="hidden text-[11px] text-muted-foreground sm:block">{e.type}</code>
                    <span className="w-16 text-right text-xs text-muted-foreground tabular-nums">{timeAgo(e.time)}</span>
                  </div>
                ))}
              </Card>
            </section>
          ))}
        </div>
      )}
    </Page>
  )
}
