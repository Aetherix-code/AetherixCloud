import { useState } from 'react'
import { Select } from '@/components/ui/input'
import { ExecTerminal } from '@/components/xterm-view'
import { ReplicaState, type Replica, type Service } from '@/gen/aetherix/v1/types_pb'
import { Empty } from '@/components/ui/states'
import { shortId } from '@/lib/utils'

export function TerminalTab({ service, replicas }: { service: Service; replicas: Replica[] }) {
  const running = replicas.filter((r) => r.state === ReplicaState.RUNNING)
  const [replica, setReplica] = useState('')
  if (!running.length) return <Empty title="No running replica">A terminal needs at least one running replica.</Empty>
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <Select className="w-auto" value={replica} onChange={(e) => setReplica(e.target.value)} aria-label="Replica">
          <option value="">First running replica</option>
          {running.map((r) => <option key={r.id} value={r.id}>{shortId(r.id)} @ {r.machineId.slice(0, 10)}</option>)}
        </Select>
        <span className="text-xs text-muted-foreground">Proxied over the mesh to the owning node.</span>
      </div>
      <ExecTerminal serviceId={service.id} replicaId={replica || undefined} className="h-[26rem] w-full overflow-hidden rounded-md border p-2" />
    </div>
  )
}
