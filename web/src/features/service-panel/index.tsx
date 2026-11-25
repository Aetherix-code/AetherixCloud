import { X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import type { Service } from '@/gen/aetherix/v1/types_pb'
import { useService } from '@/lib/queries'
import { ReplicaBadge } from '@/lib/status'
import { DeploymentsTab } from './deployments'
import { LogsTab } from './logs'
import { MetricsTab } from './metrics'
import { SettingsTab } from './settings'
import { TerminalTab } from './terminal'
import { VariablesTab } from './variables'

export const SERVICE_TABS = ['deployments', 'variables', 'metrics', 'logs', 'settings', 'terminal'] as const

export function ServicePanel({ service, environmentId, services, tab, onTab, onClose }: {
  service: Service
  environmentId: string
  services: Service[]
  tab: string
  onTab: (t: string) => void
  onClose: () => void
}) {
  const detail = useService(service.id)
  const live = detail.data?.service ?? service
  const replicas = detail.data?.replicas ?? []
  const domains = detail.data?.domains ?? []
  return (
    <aside className="flex w-full max-w-[640px] shrink-0 flex-col border-l bg-card md:w-[560px] lg:w-[640px]" aria-label={`${service.name} panel`}>
      <div className="flex items-start gap-3 border-b px-4 py-3">
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-base font-semibold">{live.name}</h2>
          <div className="mt-1.5 flex flex-wrap gap-1.5">
            {replicas.map((r) => <ReplicaBadge key={r.id} state={r.state} />)}
          </div>
        </div>
        <Button variant="ghost" size="icon" onClick={onClose} aria-label="Close panel"><X /></Button>
      </div>
      <Tabs value={SERVICE_TABS.includes(tab as never) ? tab : 'deployments'} onValueChange={onTab} className="flex min-h-0 flex-1 flex-col">
        <TabsList>
          {SERVICE_TABS.map((t) => <TabsTrigger key={t} value={t}>{t[0].toUpperCase() + t.slice(1)}</TabsTrigger>)}
        </TabsList>
        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          <TabsContent value="deployments"><DeploymentsTab service={live} /></TabsContent>
          <TabsContent value="variables"><VariablesTab service={live} environmentId={environmentId} services={services} /></TabsContent>
          <TabsContent value="metrics"><MetricsTab service={live} replicas={replicas} /></TabsContent>
          <TabsContent value="logs"><LogsTab service={live} replicas={replicas} /></TabsContent>
          <TabsContent value="settings"><SettingsTab service={live} replicas={replicas} domains={domains} /></TabsContent>
          <TabsContent value="terminal"><TerminalTab service={live} replicas={replicas} /></TabsContent>
        </div>
      </Tabs>
    </aside>
  )
}
