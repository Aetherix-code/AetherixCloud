import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty } from '@/components/ui/states'
import { ReplicaState, type Replica, type Service } from '@/gen/aetherix/v1/types_pb'
import { isMock } from '@/lib/api'
import { fetchMetrics } from '@/lib/extras'
import { formatBytes, formatCpu } from '@/lib/utils'
import { Activity } from 'lucide-react'

type Point = { t: number; value: number }

/** Deterministic pseudo-series so the mock charts are stable between renders. */
export function series(seed: string, n = 48, base = 40, amp = 25): number[] {
  let h = 0
  for (const c of seed) h = (h * 31 + c.charCodeAt(0)) >>> 0
  return Array.from({ length: n }, (_, i) => {
    h = (h * 1664525 + 1013904223) >>> 0
    return Math.max(2, Math.min(98, base + Math.sin(i / 5) * amp * 0.6 + ((h % 1000) / 1000 - 0.5) * amp))
  })
}

function mockPoints(seed: string, n = 48, base = 40, amp = 25, scale = 1): Point[] {
  const now = Date.now()
  const step = 10_000
  return series(seed, n, base, amp).map((v, i) => ({
    t: now - (n - 1 - i) * step,
    value: v * scale,
  }))
}

function zipPoints(times: number[] | undefined, values: number[] | undefined): Point[] {
  if (!values?.length) return []
  const n = values.length
  if (times?.length === n) {
    return values.map((value, i) => ({ t: times[i], value }))
  }
  const now = Date.now()
  const step = 10_000
  return values.map((value, i) => ({ t: now - (n - 1 - i) * step, value }))
}

function formatTime(t: number): string {
  return new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

function MetricChart({
  data,
  formatValue,
  unitHint,
  domain,
}: {
  data: Point[]
  formatValue: (v: number) => string
  unitHint?: string
  domain?: [number | 'auto', number | 'auto']
}) {
  if (!data.length) return <div className="h-44 w-full" />

  return (
    <div className="h-44 w-full">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
          <defs>
            <linearGradient id="metricFill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--primary)" stopOpacity={0.28} />
              <stop offset="100%" stopColor="var(--primary)" stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid stroke="var(--border)" strokeDasharray="3 3" vertical={false} />
          <XAxis
            dataKey="t"
            type="number"
            domain={['dataMin', 'dataMax']}
            tickFormatter={(v) => formatTime(Number(v)).replace(/:\d{2}$/, '')}
            tick={{ fill: 'var(--muted-foreground)', fontSize: 11 }}
            axisLine={{ stroke: 'var(--border)' }}
            tickLine={false}
            minTickGap={40}
          />
          <YAxis
            width={56}
            domain={domain ?? ['auto', 'auto']}
            tickFormatter={(v) => formatValue(Number(v))}
            tick={{ fill: 'var(--muted-foreground)', fontSize: 11 }}
            axisLine={false}
            tickLine={false}
          />
          <Tooltip
            cursor={{ stroke: 'var(--primary)', strokeOpacity: 0.35 }}
            content={({ active, payload }) => {
              if (!active || !payload?.length) return null
              const point = payload[0]?.payload as Point | undefined
              if (!point) return null
              return (
                <div className="rounded-md border bg-card px-2.5 py-1.5 text-xs shadow-sm">
                  <div className="text-muted-foreground">{formatTime(point.t)}</div>
                  <div className="mt-0.5 font-medium tabular-nums">
                    {formatValue(point.value)}
                    {unitHint ? <span className="ml-1 font-normal text-muted-foreground">{unitHint}</span> : null}
                  </div>
                </div>
              )
            }}
          />
          <Area
            type="monotone"
            dataKey="value"
            stroke="var(--primary)"
            strokeWidth={1.75}
            fill="url(#metricFill)"
            isAnimationActive={false}
            activeDot={{ r: 4, strokeWidth: 0, fill: 'var(--primary)' }}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}

function latestLabel(data: Point[], formatValue: (v: number) => string): string {
  if (!data.length) return '—'
  return formatValue(data[data.length - 1].value)
}

export function MetricsTab({ service, replicas }: { service: Service; replicas: Replica[] }) {
  const res = service.spec?.resources
  const running = replicas.filter((r) => r.state === ReplicaState.RUNNING).length
  const q = useQuery({
    queryKey: ['metrics', service.id],
    queryFn: () => fetchMetrics(service.id),
    refetchInterval: 10_000,
    enabled: !isMock,
  })
  const agg = q.data?.aggregate
  const hasLive = !!(agg && (agg.cpu_percent?.length || agg.memory_usage?.length))

  const cpu = useMemo(
    () => (isMock ? mockPoints(service.id + 'CPU') : zipPoints(agg?.times, agg?.cpu_percent)),
    [agg?.cpu_percent, agg?.times, service.id],
  )
  const memory = useMemo(
    () =>
      isMock
        ? mockPoints(service.id + 'Memory', 48, 55, 20, 16 * 1024 * 1024)
        : zipPoints(agg?.times, agg?.memory_usage),
    [agg?.memory_usage, agg?.times, service.id],
  )
  const network = useMemo(
    () =>
      isMock
        ? mockPoints(service.id + 'Network', 48, 40, 25, 64 * 1024)
        : zipPoints(agg?.times, agg?.net_rx_bytes),
    [agg?.net_rx_bytes, agg?.times, service.id],
  )

  return (
    <div className="space-y-4">
      {!isMock && !hasLive && !q.isLoading && (
        <Card className="border-dashed">
          <Empty title="Waiting for live samples" icon={<Activity className="size-8" />}>
            Per-service CPU, memory and network are sampled on each node every 10s. Charts appear once a replica is running on a node with Docker.
          </Empty>
        </Card>
      )}
      <div className="grid grid-cols-3 gap-3 text-sm">
        <Card><CardContent><div className="text-xs text-muted-foreground">Replicas</div><div className="text-lg font-semibold">{running}/{service.spec?.replicas ?? 0}</div></CardContent></Card>
        <Card><CardContent><div className="text-xs text-muted-foreground">CPU limit</div><div className="text-lg font-semibold">{res?.cpuMillis ? formatCpu(res.cpuMillis) : '—'}</div></CardContent></Card>
        <Card><CardContent><div className="text-xs text-muted-foreground">Memory limit</div><div className="text-lg font-semibold">{res?.memoryBytes ? formatBytes(Number(res.memoryBytes)) : '—'}</div></CardContent></Card>
      </div>
      {(isMock || hasLive) && (
        <>
          <Card>
            <CardHeader>
              <CardTitle className="flex items-baseline justify-between gap-3">
                <span>CPU <span className="font-normal text-muted-foreground">· {isMock ? 'mock' : 'recent'}</span></span>
                <span className="text-sm font-semibold tabular-nums text-foreground">{latestLabel(cpu, (v) => `${v.toFixed(1)}%`)}</span>
              </CardTitle>
            </CardHeader>
            <CardContent>
              <MetricChart data={cpu} formatValue={(v) => `${v.toFixed(1)}%`} domain={[0, 'auto']} />
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle className="flex items-baseline justify-between gap-3">
                <span>Memory <span className="font-normal text-muted-foreground">· {isMock ? 'mock' : 'recent'}</span></span>
                <span className="text-sm font-semibold tabular-nums text-foreground">{latestLabel(memory, formatBytes)}</span>
              </CardTitle>
            </CardHeader>
            <CardContent>
              <MetricChart data={memory} formatValue={formatBytes} domain={[0, 'auto']} />
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle className="flex items-baseline justify-between gap-3">
                <span>Network <span className="font-normal text-muted-foreground">· rx</span></span>
                <span className="text-sm font-semibold tabular-nums text-foreground">{latestLabel(network, formatBytes)}</span>
              </CardTitle>
            </CardHeader>
            <CardContent>
              <MetricChart data={network} formatValue={formatBytes} unitHint="rx" domain={[0, 'auto']} />
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}
