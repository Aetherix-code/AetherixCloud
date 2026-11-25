// In-browser mock backend (VITE_MOCK=1): the real service descriptors served by
// an in-memory implementation through connect's router transport, so every
// page exercises the same client code paths as against a real node.
import { create } from '@bufbuild/protobuf'
import { timestampFromDate, timestampNow } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport, type Transport } from '@connectrpc/connect'
import { AuditEntrySchema, AuthService, MeResponseSchema } from '@/gen/aetherix/v1/auth_pb'
import { ClusterService } from '@/gen/aetherix/v1/cluster_pb'
import { DeployService } from '@/gen/aetherix/v1/deploy_pb'
import { GitHubService } from '@/gen/aetherix/v1/github_pb'
import { LogEntrySchema, LogService } from '@/gen/aetherix/v1/log_pb'
import { MachineService } from '@/gen/aetherix/v1/machine_pb'
import { ProjectService } from '@/gen/aetherix/v1/project_pb'
import { ServiceService } from '@/gen/aetherix/v1/service_pb'
import {
  ApiTokenSchema,
  DeploymentSchema,
  DeploymentStatus,
  DomainSchema,
  EnvironmentSchema,
  EventSchema,
  ProjectSchema,
  ReplicaSchema,
  ReplicaState,
  ServiceSchema,
  UserSchema,
  VariableSchema,
  type Deployment,
  type Event,
  type Role,
  type Service,
} from '@/gen/aetherix/v1/types_pb'
import { seed, type MockDB } from './data'

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
const notFound = (what: string, id: string) => new ConnectError(`${what} ${id} not found`, Code.NotFound)

type UpgradeState = {
  channel: string
  desiredVersion: string
  downloadBase: string
  machineIds: string[]
  startedAt?: ReturnType<typeof timestampNow>
}
let upgradeState: UpgradeState = { channel: 'latest', desiredVersion: '', downloadBase: '', machineIds: [] }

function mockUpgradeStatus(db: MockDB, state: UpgradeState) {
  const want = new Set(state.machineIds)
  const machines = db.machines
    .filter((m) => !want.size || want.has(m.id))
    .map((m) => {
      const onTarget = !!state.desiredVersion && m.agentVersion === state.desiredVersion
      return {
        machineId: m.id,
        machineName: m.name,
        agentVersion: m.agentVersion,
        arch: m.arch,
        upgradeStatus: m.upgradeStatus,
        upgradeMessage: m.upgradeMessage,
        onTarget,
      }
    })
  return {
    channel: state.channel,
    desiredVersion: state.desiredVersion,
    downloadBase: state.downloadBase,
    machineIds: state.machineIds,
    startedAt: state.startedAt,
    machines,
    onTarget: machines.filter((m) => m.onTarget).length,
    total: machines.length,
  }
}

const BUILD_LINES = [
  '#1 [internal] load build definition',
  '#2 detecting provider … railpack (node 22, pnpm)',
  '#3 [install] pnpm install --frozen-lockfile',
  '#3 Progress: resolved 412, reused 412, downloaded 0, added 412, done',
  '#4 [build] pnpm run build',
  '#4 vite v6 building for production…',
  '#4 ✓ 128 modules transformed.',
  '#5 exporting layers',
  '#5 pushing to registry.internal on node-1',
  '#6 build succeeded in 38.2s',
]
const APP_LINES = [
  'GET /healthz 200 1ms',
  'GET /api/products?limit=20 200 12ms',
  'POST /api/cart 201 18ms',
  'connected to postgres at postgres.internal:5432',
  'worker: processed job email.digest id=8841 in 220ms',
  'WARN slow query (312ms): SELECT * FROM orders WHERE …',
  'GET /api/orders/1832 200 9ms',
]

export function createMockTransport(): Transport {
  const db: MockDB = seed()
  const emit: ((e: Event) => void)[] = []
  const nid = (p: string) => `${p}_${++db.seq}`
  const pushEvent = (type: string, subjectId: string, message: string) => {
    const e = create(EventSchema, { id: nid('evt'), type, subjectId, message, time: timestampNow(), attributes: {} })
    db.events.unshift(e)
    emit.forEach((f) => f(e))
  }
  const pushAudit = (partial: {
    action: string
    summary: string
    ok?: boolean
    error?: string
    via?: string
    client?: string
    attributes?: Record<string, string>
    actorEmail?: string
    actorUserId?: string
  }) => {
    const u = db.users[0]
    db.audit.unshift(create(AuditEntrySchema, {
      id: nid('aud'),
      time: timestampNow(),
      actorEmail: partial.actorEmail ?? u?.email ?? '',
      actorUserId: partial.actorUserId ?? u?.id ?? '',
      via: partial.via ?? 'dashboard',
      client: partial.client ?? '',
      action: partial.action,
      summary: partial.summary,
      ok: partial.ok ?? true,
      error: partial.error ?? '',
      attributes: partial.attributes ?? {},
    }))
  }
  const svcOf = (id: string): Service => db.services.find((s) => s.id === id) ?? (() => { throw notFound('service', id) })()
  const mach = (ref: string) => db.machines.find((m) => m.id === ref || m.name === ref) ?? (() => { throw notFound('machine', ref) })()
  const proj = (ref: string) => db.projects.find((p) => p.id === ref || p.name === ref) ?? (() => { throw notFound('project', ref) })()
  const prodEnv = (projectId: string) => db.environments.find((e) => e.projectId === projectId && !e.ephemeral)

  /** Simulate queued -> building -> deploying -> active. */
  function runDeployment(d: Deployment) {
    const step = (status: DeploymentStatus, after: number) =>
      setTimeout(() => {
        d.status = status
        if (status === DeploymentStatus.ACTIVE) {
          d.finishedAt = timestampNow()
          const s = db.services.find((x) => x.id === d.serviceId)
          for (const o of db.deployments) if (o.serviceId === d.serviceId && o.id !== d.id && o.status === DeploymentStatus.ACTIVE) o.status = DeploymentStatus.SUPERSEDED
          if (s) s.activeDeploymentId = d.id
        }
        pushEvent('deployment.status', d.id, `${db.services.find((x) => x.id === d.serviceId)?.name}: deployment ${d.commitSha} is now ${DeploymentStatus[status].toLowerCase()}`)
      }, after)
    step(DeploymentStatus.BUILDING, 400)
    step(DeploymentStatus.DEPLOYING, 6000)
    step(DeploymentStatus.ACTIVE, 9000)
  }
  // The seeded in-flight build progresses too.
  setTimeout(() => {
    const d = db.deployments.find((x) => x.id === 'dep_api_3')
    if (d) runDeployment(d)
  }, 2000)

  function newDeployment(serviceId: string, sha: string, message: string, previous?: string, digest?: string): Deployment {
    const s = svcOf(serviceId)
    const d = create(DeploymentSchema, {
      id: nid('dep'),
      serviceId,
      spec: s.spec,
      status: DeploymentStatus.QUEUED,
      commitSha: sha,
      commitMessage: message,
      author: 'you',
      imageDigest: digest ?? '',
      buildId: s.spec?.source?.github ? nid('bld') : '',
      previousDeploymentId: previous ?? s.activeDeploymentId,
      createdAt: timestampNow(),
    })
    db.deployments.unshift(d)
    runDeployment(d)
    return d
  }

  const authed = () => create(UserSchema, db.users[0])

  return createRouterTransport(({ service }) => {
    service(ClusterService, {
      getCluster: () => ({ cluster: db.cluster }),
      getStatus: () => ({
        cluster: db.cluster,
        machinesTotal: db.machines.length,
        machinesUp: db.machines.filter((m) => m.state === 1).length,
        machinesSuspect: db.machines.filter((m) => m.state === 2).length,
        machinesDown: db.machines.filter((m) => m.state === 3).length,
        servicesTotal: db.services.length,
        replicasRunning: db.replicas.filter((r) => r.state === ReplicaState.RUNNING).length,
        replicasDesired: db.services.reduce((n, s) => n + (s.spec?.replicas ?? 0), 0),
        servedBy: 'node-1 (mock)',
        version: 'mock',
      }),
      updateCluster: (r) => {
        if (r.name !== undefined) db.cluster.name = r.name
        if (r.domain !== undefined) db.cluster.domain = r.domain
        return { cluster: db.cluster }
      },
      checkUpdates: (r) => ({
        currentVersion: 'mock',
        latestVersion: 'v0.4.0',
        channel: r.channel || upgradeState.channel,
        releaseUrl: 'https://github.com/Aetherix-code/AetherixCloud/releases/tag/v0.1.0',
        updateAvailable: true,
        releaseNotes: 'Mock release notes for the Updates page.',
      }),
      getUpgrade: () => mockUpgradeStatus(db, upgradeState),
      startUpgrade: (r) => {
        upgradeState = {
          channel: r.channel || upgradeState.channel || 'latest',
          desiredVersion: r.version,
          downloadBase: r.downloadBase || '',
          machineIds: [...(r.machineIds ?? [])],
          startedAt: timestampNow(),
        }
        for (const m of db.machines) {
          if (upgradeState.machineIds.length && !upgradeState.machineIds.includes(m.id)) continue
          m.upgradeStatus = 'downloading'
          m.upgradeMessage = `fetching ${r.version}`
        }
        pushAudit({
          action: '/aetherix.v1.ClusterService/StartUpgrade',
          summary: `upgrade to ${r.version}`,
          attributes: { version: r.version },
        })
        return { status: mockUpgradeStatus(db, upgradeState) }
      },
      cancelUpgrade: () => {
        upgradeState = { channel: upgradeState.channel || 'latest', desiredVersion: '', downloadBase: '', machineIds: [] }
        for (const m of db.machines) {
          m.upgradeStatus = ''
          m.upgradeMessage = ''
        }
        return { status: mockUpgradeStatus(db, upgradeState) }
      },
      listEvents: (r) => ({ events: db.events.filter((e) => e.type.startsWith(r.typePrefix)).slice(0, r.limit || 100) }),
      async *watchEvents(r, ctx) {
        const queue: Event[] = []
        let wake: (() => void) | undefined
        const f = (e: Event) => {
          queue.push(e)
          wake?.()
        }
        emit.push(f)
        // Ambient activity so the live feed moves during a demo.
        const ambient = setInterval(() => {
          const s = db.services[Math.floor(Math.random() * db.services.length)]
          pushEvent('replica.state', s.id, `${s.name}: replica health check passed`)
        }, 20000)
        try {
          for (const e of db.events.slice(0, r.replay).reverse()) yield e
          while (!ctx.signal.aborted) {
            if (!queue.length) await new Promise<void>((res) => { wake = res; ctx.signal.addEventListener('abort', () => res(), { once: true }) })
            while (queue.length) yield queue.shift()!
          }
        } finally {
          clearInterval(ambient)
          emit.splice(emit.indexOf(f), 1)
        }
      },
      async *backup() {
        yield { data: new TextEncoder().encode('{"version":2,"tables":{},"identity":{}}\n') }
      },
      async restore(_req: AsyncIterable<{ data: Uint8Array; force: boolean }>) {
        // Consume the upload so the client stream completes.
        for await (const _ of _req) { /* drain */ }
        return { tables: 0, rows: 0, machinesRemoved: 0, replicasCleared: 0 }
      },
    })

    service(MachineService, {
      listMachines: () => ({ machines: db.machines }),
      getMachine: (r) => ({ machine: mach(r.machine) }),
      createJoinToken: (r) => {
        const token = 'axj_' + Math.random().toString(36).slice(2, 12) + Math.random().toString(36).slice(2, 12)
        pushAudit({
          action: '/aetherix.v1.MachineService/CreateJoinToken',
          summary: `join token${r.machineName ? ` for ${r.machineName}` : ''}`,
          attributes: r.machineName ? { machine_name: r.machineName } : {},
        })
        return {
          token,
          installCommand: `curl -fsSL https://${db.cluster.domain}/install.sh | sudo sh -s -- --join ${token}${r.machineName ? ` --name ${r.machineName}` : ''}`,
          expiresUnix: BigInt(Math.floor(Date.now() / 1000) + (r.ttlSeconds || 3600)),
        }
      },
      joinMachine: () => { throw new ConnectError('mock', Code.Unimplemented) },
      updateMachine: (r) => {
        const m = mach(r.machine)
        if (r.name !== undefined) m.name = r.name
        if (r.setRoles) m.roles = r.roles
        if (r.setLabels) m.labels = r.labels
        pushAudit({
          action: '/aetherix.v1.MachineService/UpdateMachine',
          summary: `updated ${m.name}`,
          attributes: { machine: m.id, name: m.name },
        })
        return { machine: m }
      },
      cordonMachine: (r) => {
        const m = mach(r.machine)
        m.cordoned = r.cordoned
        pushEvent('machine.cordon', m.id, `${m.name} ${r.cordoned ? 'cordoned' : 'uncordoned'}`)
        pushAudit({
          action: '/aetherix.v1.MachineService/CordonMachine',
          summary: `${r.cordoned ? 'cordoned' : 'uncordoned'} ${m.name}`,
          attributes: { machine: m.id },
        })
        return { machine: m }
      },
      drainMachine: (r) => {
        const m = mach(r.machine)
        m.cordoned = true
        const moved = db.replicas.filter((x) => x.machineId === m.id && x.state === ReplicaState.RUNNING)
        const target = db.machines.find((x) => x.id !== m.id && x.state === 1)
        for (const x of moved) if (target) x.machineId = target.id
        pushEvent('machine.drain', m.id, `${m.name} drained (${moved.length} replicas moved)`)
        pushAudit({
          action: '/aetherix.v1.MachineService/DrainMachine',
          summary: `drained ${m.name}`,
          attributes: { machine: m.id },
        })
        return { machine: m, replicasMoved: moved.length, strandedServiceIds: db.replicas.filter((x) => x.machineId === m.id && x.state === ReplicaState.STRANDED).map((x) => x.serviceId) }
      },
      removeMachine: (r) => {
        const m = mach(r.machine)
        db.machines = db.machines.filter((x) => x !== m)
        pushEvent('machine.remove', m.id, `${m.name} removed`)
        pushAudit({
          action: '/aetherix.v1.MachineService/RemoveMachine',
          summary: `removed ${m.name}`,
          attributes: { machine: m.id },
        })
        return {}
      },
      reportHeartbeat: () => ({}),
    })

    service(ProjectService, {
      listProjects: () => ({ projects: db.projects }),
      getProject: (r) => {
        const p = proj(r.project)
        return { project: p, environments: db.environments.filter((e) => e.projectId === p.id), services: db.services.filter((s) => s.projectId === p.id) }
      },
      createProject: (r) => {
        const p = create(ProjectSchema, { id: nid('prj'), name: r.name, description: r.description, createdAt: timestampNow() })
        const e = create(EnvironmentSchema, { id: nid('env'), projectId: p.id, name: 'default', createdAt: timestampNow() })
        db.projects.push(p)
        db.environments.push(e)
        return { project: p, environment: e }
      },
      deleteProject: (r) => {
        const p = proj(r.project)
        db.projects = db.projects.filter((x) => x !== p)
        db.services = db.services.filter((s) => s.projectId !== p.id)
        return {}
      },
      createEnvironment: (r) => {
        const e = create(EnvironmentSchema, { id: nid('env'), projectId: proj(r.project).id, name: r.name, ephemeral: r.ephemeral, createdAt: timestampNow() })
        db.environments.push(e)
        return { environment: e }
      },
      listEnvironments: (r) => ({ environments: db.environments.filter((e) => e.projectId === proj(r.project).id) }),
      deleteEnvironment: (r) => {
        db.environments = db.environments.filter((e) => e.id !== r.environmentId)
        return {}
      },
      listVariables: (r) => ({
        variables: db.variables.filter((v) => v.environmentId === r.environmentId && (r.serviceId === '*' || v.serviceId === r.serviceId)),
      }),
      setVariables: (r) => {
        for (const nv of r.variables) {
          const ex = db.variables.find((v) => v.environmentId === r.environmentId && v.serviceId === r.serviceId && v.key === nv.key)
          if (ex) {
            if (!(nv.redacted && !nv.value)) ex.value = nv.value
            ex.secret = nv.secret
            ex.redacted = nv.secret && !!ex.value
          } else db.variables.push(create(VariableSchema, { ...nv, id: nid('var'), environmentId: r.environmentId, serviceId: r.serviceId }))
        }
        if (r.redeploy) newDeployment(r.serviceId, 'vars', 'Variables changed')
        return { variables: db.variables.filter((v) => v.serviceId === r.serviceId) }
      },
      deleteVariable: (r) => {
        db.variables = db.variables.filter((v) => !(v.environmentId === r.environmentId && v.serviceId === r.serviceId && v.key === r.key))
        return {}
      },
    })

    service(ServiceService, {
      listServices: (r) => ({ services: db.services.filter((s) => !r.project || s.projectId === proj(r.project).id) }),
      getService: (r) => {
        const s = svcOf(r.service)
        return { service: s, replicas: db.replicas.filter((x) => x.serviceId === s.id), domains: db.domains.filter((d) => d.serviceId === s.id) }
      },
      createService: (r) => {
        const p = proj(r.project)
        const env = db.environments.find((e) => e.id === r.environment || e.name === r.environment) ?? prodEnv(p.id)
        const s = create(ServiceSchema, { id: nid('svc'), projectId: p.id, environmentId: env?.id ?? '', name: r.name, spec: r.spec, createdAt: timestampNow(), updatedAt: timestampNow() })
        if (s.spec && !s.spec.replicas) s.spec.replicas = 1
        db.services.push(s)
        const d = r.deploy ? newDeployment(s.id, r.spec?.source?.github ? 'HEAD' : 'image', 'Initial deploy') : undefined
        db.replicas.push(create(ReplicaSchema, { id: nid('rep'), serviceId: s.id, deploymentId: d?.id ?? '', machineId: db.machines[0].id, containerId: 'c', state: ReplicaState.STARTING, desiredRunning: true, updatedAt: timestampNow() }))
        return { service: s, deployment: d }
      },
      updateService: (r) => {
        const s = svcOf(r.serviceId)
        if (r.spec && s.spec) {
          const camel = (n: string) => n.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase())
          const target = s.spec as unknown as Record<string, unknown>
          const src = r.spec as unknown as Record<string, unknown>
          if (r.updateMask.length) for (const m of r.updateMask) target[camel(m)] = src[camel(m)]
          else s.spec = r.spec
        } else if (r.spec) s.spec = r.spec
        s.updatedAt = timestampNow()
        return { service: s }
      },
      deleteService: (r) => {
        db.services = db.services.filter((s) => s.id !== r.serviceId)
        return {}
      },
      scaleService: (r) => {
        const s = svcOf(r.serviceId)
        if (s.spec) s.spec.replicas = r.replicas
        return { service: s }
      },
      restartService: (r) => {
        pushEvent('service.restart', r.serviceId, `${svcOf(r.serviceId).name} restarted`)
        return {}
      },
      listReplicas: (r) => ({ replicas: db.replicas.filter((x) => (!r.serviceId || x.serviceId === r.serviceId) && (!r.machineId || x.machineId === r.machineId)) }),
      listDomains: (r) => ({ domains: db.domains.filter((d) => d.serviceId === r.serviceId) }),
      addDomain: (r) => {
        const d = create(DomainSchema, { id: nid('dom'), serviceId: r.serviceId, hostname: r.hostname, port: r.port, tlsState: 'pending', tlsExpiresAt: timestampFromDate(new Date(Date.now() + 86400000 * 90)) })
        db.domains.push(d)
        return { domain: d, dnsHints: [`${r.hostname}. CNAME web-shop.203-0-113-1.sslip.io.`, `or  A  203.0.113.1`] }
      },
      removeDomain: (r) => {
        db.domains = db.domains.filter((d) => d.id !== r.domainId)
        return {}
      },
      // eslint-disable-next-line require-yield
      async *exec() {
        throw new ConnectError('exec uses lib/exec in mock mode', Code.Unimplemented)
      },
    })

    service(DeployService, {
      deploy: (r) => ({ deployment: newDeployment(r.serviceId, r.commitSha || 'manual', r.redeploy ? 'Redeploy' : 'Manual deploy') }),
      deployCompose: () => ({ services: [], deployments: [], warnings: ['mock: compose deploy is a no-op'], removed: [] }),
      getDeployment: (r) => {
        const d = db.deployments.find((x) => x.id === r.deploymentId) ?? (() => { throw notFound('deployment', r.deploymentId) })()
        return { deployment: d, replicas: db.replicas.filter((x) => x.deploymentId === d.id) }
      },
      listDeployments: (r) => ({ deployments: db.deployments.filter((d) => d.serviceId === r.serviceId).slice(0, r.limit || 50) }),
      rollback: (r) => {
        const target = db.deployments.find((d) => d.id === r.deploymentId) ?? (() => { throw notFound('deployment', r.deploymentId) })()
        const d = newDeployment(r.serviceId, target.commitSha, `Rollback to ${target.commitSha}`, undefined, target.imageDigest)
        return { deployment: d }
      },
      cancelDeployment: (r) => {
        const d = db.deployments.find((x) => x.id === r.deploymentId)
        if (d) d.status = DeploymentStatus.FAILED
        return {}
      },
      async *watchDeployment(r, ctx) {
        while (!ctx.signal.aborted) {
          const d = db.deployments.find((x) => x.id === r.deploymentId)
          if (!d) return
          yield d
          if (d.status !== DeploymentStatus.BUILDING && d.status !== DeploymentStatus.DEPLOYING && d.status !== DeploymentStatus.QUEUED) return
          await sleep(1000)
        }
      },
    })

    service(LogService, {
      async *streamLogs(r, ctx) {
        const reps = db.replicas.filter((x) => x.serviceId === r.serviceId && (!r.replicaId || x.id === r.replicaId))
        const mk = (rep: (typeof reps)[number], line: string) =>
          create(LogEntrySchema, { time: timestampNow(), replicaId: rep.id, machineId: rep.machineId, stream: line.startsWith('WARN') ? 'stderr' : 'stdout', line })
        for (let i = 0; i < Math.min(r.tail || 20, 20) && reps.length; i++) yield mk(reps[i % reps.length], APP_LINES[i % APP_LINES.length])
        while (r.follow && reps.length && !ctx.signal.aborted) {
          await sleep(900)
          yield mk(reps[Math.floor(Math.random() * reps.length)], APP_LINES[Math.floor(Math.random() * APP_LINES.length)])
        }
      },
      async *streamBuildLogs(r, ctx) {
        for (const line of BUILD_LINES) {
          if (ctx.signal.aborted) return
          yield create(LogEntrySchema, { time: timestampNow(), stream: 'stdout', line: `[${r.buildId}] ${line}` })
          await sleep(r.follow ? 500 : 0)
        }
      },
    })

    // The real backend answers Unimplemented for these today; the mock
    // implements them so the UI flows are demoable.
    service(AuthService, {
      login: (r) => {
        if (!r.password) throw new ConnectError('invalid credentials', Code.Unauthenticated)
        return { sessionToken: 'mock-session', user: authed(), expiresUnix: BigInt(Math.floor(Date.now() / 1000) + 86400) }
      },
      logout: () => ({}),
      me: () => create(MeResponseSchema, { user: authed(), scopes: ['read', 'deploy', 'admin'] }),
      createUser: (r) => {
        const u = create(UserSchema, { id: nid('usr'), email: r.email, name: r.name, role: r.role as Role, createdAt: timestampNow() })
        db.users.push(u)
        pushAudit({
          action: '/aetherix.v1.AuthService/CreateUser',
          summary: `invited ${r.email}`,
          attributes: { email: r.email, role: String(r.role) },
        })
        return { user: u }
      },
      listUsers: () => ({ users: db.users }),
      updateUser: (r) => {
        const u = db.users.find((x) => x.id === r.userId) ?? (() => { throw notFound('user', r.userId) })()
        if (r.name !== undefined) u.name = r.name
        if (r.role !== undefined) u.role = r.role
        pushAudit({
          action: '/aetherix.v1.AuthService/UpdateUser',
          summary: `updated ${u.email}`,
          attributes: { user_id: u.id, ...(r.role !== undefined ? { role: String(r.role) } : {}) },
        })
        return { user: u }
      },
      deleteUser: (r) => {
        const u = db.users.find((x) => x.id === r.userId)
        db.users = db.users.filter((x) => x.id !== r.userId)
        pushAudit({
          action: '/aetherix.v1.AuthService/DeleteUser',
          summary: `deleted ${u?.email ?? r.userId}`,
          attributes: { user_id: r.userId },
        })
        return {}
      },
      createApiToken: (r) => {
        const t = create(ApiTokenSchema, { id: nid('tok'), name: r.name, scopes: r.scopes, userId: db.users[0].id, createdAt: timestampNow() })
        db.tokens.push(t)
        pushAudit({
          action: '/aetherix.v1.AuthService/CreateApiToken',
          summary: `created token ${r.name}`,
          attributes: { name: r.name },
        })
        return { token: t, plaintext: 'axt_' + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2) }
      },
      listApiTokens: () => ({ tokens: db.tokens }),
      getSetupStatus: () => ({ needsSetup: db.needsSetup }),
      bootstrap: (r) => {
        if (!db.needsSetup) throw new ConnectError('already set up', Code.FailedPrecondition)
        db.needsSetup = false
        db.users[0].email = r.email
        db.users[0].name = r.name
        pushAudit({ action: 'auth.bootstrap', summary: `bootstrapped as ${r.email}`, attributes: { email: r.email } })
        return { sessionToken: 'mock-session', user: authed(), expiresUnix: BigInt(Math.floor(Date.now() / 1000) + 86400) }
      },
      listAuditLog: (r) => ({
        entries: db.audit
          .filter((e) => (!r.actionPrefix || e.action.startsWith(r.actionPrefix) || e.action.includes(r.actionPrefix)) && (!r.actorUserId || e.actorUserId === r.actorUserId))
          .slice(0, r.limit || 100),
      }),
      listOAuthGrants: () => ({ grants: db.grants }),
      revokeOAuthGrant: (r) => {
        db.grants = db.grants.filter((g) => g.id !== r.grantId)
        pushAudit({
          action: '/aetherix.v1.AuthService/RevokeOAuthGrant',
          summary: `revoked grant ${r.grantId}`,
          attributes: { grant_id: r.grantId },
        })
        return {}
      },
      revokeApiToken: (r) => {
        db.tokens = db.tokens.filter((t) => t.id !== r.tokenId)
        pushAudit({
          action: '/aetherix.v1.AuthService/RevokeApiToken',
          summary: `revoked token ${r.tokenId}`,
          attributes: { token_id: r.tokenId },
        })
        return {}
      },
    })

    service(GitHubService, {
      getApp: () => ({ configured: db.github.configured, appId: db.github.appId, slug: db.github.slug, htmlUrl: db.github.htmlUrl, webhookUrl: db.github.webhookUrl || `https://${db.cluster.domain}/api/github/webhook` }),
      beginManifestSetup: (r) => ({
        manifestJson: JSON.stringify({ name: `aetherix-${db.cluster.name}`, url: r.baseUrl, hook_attributes: { url: `${r.baseUrl}/api/github/webhook` }, redirect_url: `${r.baseUrl}/settings/github`, public: false, default_permissions: { contents: 'read', statuses: 'write', pull_requests: 'read' }, default_events: ['push', 'pull_request'] }, null, 2),
        postUrl: r.organization ? `https://github.com/organizations/${r.organization}/settings/apps/new` : 'https://github.com/settings/apps/new',
        state: 'mock-state',
      }),
      completeManifestSetup: () => {
        db.github = { configured: true, appId: 424242n, slug: `aetherix-${db.cluster.name}`, htmlUrl: `https://github.com/apps/aetherix-${db.cluster.name}`, webhookUrl: `https://${db.cluster.domain}/api/github/webhook` }
        return { app: { ...db.github } }
      },
      listInstallations: () => ({ installations: db.github.configured ? [{ id: 1n, account: 'acme', accountType: 'Organization', createdAt: timestampNow() }] : [] }),
      listRepositories: (r) => ({
        repositories: ['acme/shop-web', 'acme/shop-api', 'acme/shop-worker', 'acme/blog', 'acme/infra'].filter((n) => n.includes(r.query)).map((fullName) => ({ fullName, defaultBranch: 'main', private: fullName !== 'acme/blog', installationId: 1n })),
      }),
      listBranches: () => ({ branches: ['main', 'develop', 'feature/search'] }),
      simulatePush: (r) => {
        const d = newDeployment(r.serviceId, Math.random().toString(16).slice(2, 9), 'Simulated push')
        return { buildId: d.buildId, commitSha: d.commitSha }
      },
      setWebhookUrl: (r) => {
        db.github.webhookUrl = r.url
        return {}
      },
    })
  })
}
