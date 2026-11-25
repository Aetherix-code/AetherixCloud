import { useState } from 'react'
import { Select } from '@/components/ui/input'
import { LogView } from '@/components/xterm-view'
import type { Replica, Service } from '@/gen/aetherix/v1/types_pb'
import { api } from '@/lib/api'
import { shortId } from '@/lib/utils'

export function LogsTab({ service, replicas }: { service: Service; replicas: Replica[] }) {
  const [replica, setReplica] = useState('')
  const [tail, setTail] = useState(100)
  return (
    <div className="space-y-3">
      <div className="flex gap-2">
        <Select className="w-auto" value={replica} onChange={(e) => setReplica(e.target.value)} aria-label="Replica">
          <option value="">All replicas</option>
          {replicas.map((r) => <option key={r.id} value={r.id}>{shortId(r.id)} @ {r.machineId.slice(0, 10)}</option>)}
        </Select>
        <Select className="w-auto" value={tail} onChange={(e) => setTail(Number(e.target.value))} aria-label="Tail">
          {[50, 100, 500, 1000].map((n) => <option key={n} value={n}>last {n} lines</option>)}
        </Select>
      </div>
      <LogView
        className="h-[28rem] w-full overflow-hidden rounded-md border p-2"
        deps={[service.id, replica, tail]}
        open={async (emit, signal) => {
          let n = 0
          for await (const e of api.logs.streamLogs({ serviceId: service.id, replicaId: replica, follow: true, tail }, { signal })) {
            emit(`\x1b[2m${shortId(e.replicaId)}\x1b[0m ${e.line}`, e.stream)
            n++
          }
          if (!n) emit('\x1b[2mNo log output.\x1b[0m')
        }}
      />
    </div>
  )
}
