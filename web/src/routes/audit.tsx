import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Page, PageHeader, SectionLabel } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import { Input, Select } from '@/components/ui/input'
import { Empty, ListSkeleton, NotAvailable, QueryError } from '@/components/ui/states'
import { auditActionLabel, auditTarget, matchesAuditFilter, shortAction, type AuditCategory } from '@/lib/audit'
import { useAudit } from '@/lib/queries'
import { timeAgo, tsDate } from '@/lib/utils'

const CATEGORIES: { value: AuditCategory; label: string }[] = [
  { value: 'all', label: 'All events' },
  { value: 'auth', label: 'Users & auth' },
  { value: 'cluster', label: 'Cluster & machines' },
  { value: 'deploy', label: 'Projects & deploys' },
  { value: 'oauth', label: 'OAuth & agents' },
  { value: 'failed', label: 'Failures only' },
]

export function AuditSettings() {
  const [category, setCategory] = useState<AuditCategory>('all')
  const [actor, setActor] = useState('')
  const [q, setQ] = useState('')
  // Fetch broadly; category/search filter client-side so the table stays snappy.
  const audit = useAudit('')
  const actors = useMemo(() => {
    const map = new Map<string, string>()
    for (const e of audit.data ?? []) {
      const id = e.actorUserId || e.actorEmail
      if (id) map.set(id, e.actorEmail || e.actorUserId)
    }
    return [...map.entries()].sort((a, b) => a[1].localeCompare(b[1]))
  }, [audit.data])

  const filtered = useMemo(
    () => (audit.data ?? []).filter((e) => matchesAuditFilter(e, category, actor, q)),
    [audit.data, category, actor, q],
  )

  return (
    <Page wide>
      <PageHeader
        title="Audit log"
        hideTitle
        description="Security and compliance trail for privileged mutations — user and role changes, machine join/remove, upgrades, restores, deletes, OAuth grants and agent tools. Operational deploy noise lives in Activity."
        actions={
          <Link to="/activity" className="text-sm text-muted-foreground underline-offset-2 hover:text-foreground hover:underline">
            View Activity feed →
          </Link>
        }
      />

      <div className="mb-4 flex flex-wrap items-end gap-2">
        <div className="min-w-[10rem] flex-1">
          <SectionLabel>Search</SectionLabel>
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Actor, action, target…"
            aria-label="Search audit log"
          />
        </div>
        <div>
          <SectionLabel>Category</SectionLabel>
          <Select className="w-auto min-w-[11rem]" value={category} onChange={(e) => setCategory(e.target.value as AuditCategory)} aria-label="Filter by category">
            {CATEGORIES.map((c) => <option key={c.value} value={c.value}>{c.label}</option>)}
          </Select>
        </div>
        <div>
          <SectionLabel>Actor</SectionLabel>
          <Select className="w-auto min-w-[12rem]" value={actor} onChange={(e) => setActor(e.target.value)} aria-label="Filter by actor">
            <option value="">All actors</option>
            {actors.map(([id, label]) => <option key={id} value={id}>{label}</option>)}
          </Select>
        </div>
      </div>

      {audit.isLoading ? (
        <ListSkeleton rows={6} />
      ) : audit.error ? (
        <QueryError error={audit.error} what="the audit log" />
      ) : audit.data === null ? (
        <NotAvailable what="The audit log" />
      ) : !filtered.length ? (
        <Card>
          <Empty title={(audit.data?.length ?? 0) ? 'No matching audit entries' : 'No audit entries yet'}>
            Privileged admin actions (role changes, machine removes, restores, upgrades) appear here once recorded.
          </Empty>
        </Card>
      ) : (
        <Card className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="border-b bg-muted/30 text-left text-[11px] uppercase tracking-[0.05em] text-muted-foreground">
              <tr>
                <th className="p-3 font-semibold">When</th>
                <th className="p-3 font-semibold">Actor</th>
                <th className="p-3 font-semibold">Action</th>
                <th className="p-3 font-semibold">Target</th>
                <th className="p-3 font-semibold">Via</th>
                <th className="p-3 font-semibold">Outcome</th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {filtered.map((e) => {
                const when = tsDate(e.time)
                return (
                  <tr key={e.id} className="ax-row align-top hover:bg-muted/35">
                    <td className="whitespace-nowrap p-3 text-xs text-muted-foreground" title={when?.toLocaleString()}>
                      {timeAgo(e.time)}
                    </td>
                    <td className="p-3">
                      <div className="font-medium tracking-tight">{e.actorEmail || e.actorUserId || '—'}</div>
                      {e.credentialId && <div className="font-mono text-[11px] text-muted-foreground">{e.credentialId}</div>}
                    </td>
                    <td className="p-3">
                      <div className="font-medium tracking-tight">{auditActionLabel(e.action)}</div>
                      <div className="font-mono text-[11px] text-muted-foreground">{shortAction(e.action)}</div>
                      {e.summary && e.summary !== auditTarget(e) && (
                        <div className="mt-0.5 text-xs text-muted-foreground">{e.summary}</div>
                      )}
                    </td>
                    <td className="max-w-[14rem] p-3">
                      <code className="break-all text-xs">{auditTarget(e)}</code>
                    </td>
                    <td className="p-3">
                      <Badge tone={e.via === 'mcp' ? 'info' : e.via === 'api' ? 'warning' : 'neutral'}>
                        {e.via || '—'}
                        {e.client ? ` · ${e.client}` : ''}
                      </Badge>
                    </td>
                    <td className="p-3">
                      {e.ok ? (
                        <Badge tone="success">ok</Badge>
                      ) : (
                        <Badge tone="danger" title={e.error || e.attributes?.error}>
                          {e.error || e.attributes?.error || 'failed'}
                        </Badge>
                      )}
                      {e.attributes?.status && (
                        <div className="mt-0.5 font-mono text-[11px] text-muted-foreground">HTTP {e.attributes.status}</div>
                      )}
                      {e.attributes?.token_kind && (
                        <div className="font-mono text-[11px] text-muted-foreground">{e.attributes.token_kind}</div>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </Card>
      )}
    </Page>
  )
}
