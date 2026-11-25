import { create } from '@bufbuild/protobuf'
import { AuditEntrySchema, OAuthGrantSchema, type AuditEntry, type OAuthGrant } from '@/gen/aetherix/v1/auth_pb'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import {
  ApiTokenSchema,
  ClusterSchema,
  DeploymentSchema,
  DeploymentStatus,
  DomainSchema,
  EnvironmentSchema,
  EventSchema,
  MachineRole,
  MachineSchema,
  MachineState,
  PortProtocol,
  ProjectSchema,
  ReplicaSchema,
  ReplicaState,
  Role,
  ServiceMode,
  ServiceSchema,
  UserSchema,
  VariableSchema,
  DeployStrategy,
  type ApiToken,
  type Cluster,
  type Deployment,
  type Domain,
  type Environment,
  type Event,
  type Machine,
  type Project,
  type Replica,
  type Service,
  type User,
  type Variable,
} from '@/gen/aetherix/v1/types_pb'

const ago = (sec: number) => timestampFromDate(new Date(Date.now() - sec * 1000))
const GiB = 1024n ** 3n

export interface MockDB {
  cluster: Cluster
  machines: Machine[]
  projects: Project[]
  environments: Environment[]
  services: Service[]
  variables: Variable[]
  deployments: Deployment[]
  replicas: Replica[]
  domains: Domain[]
  events: Event[]
  users: User[]
  tokens: ApiToken[]
  github: { configured: boolean; appId: bigint; slug: string; htmlUrl: string; webhookUrl: string }
  grants: OAuthGrant[]
  audit: AuditEntry[]
  needsSetup: boolean
  seq: number
}

function machine(id: string, name: string, ip: number, state: MachineState, roles: MachineRole[], used: [number, number], extra: { cordoned?: boolean; agentVersion?: string } = {}): Machine {
  return create(MachineSchema, {
    id,
    name,
    publicKey: `wg-${id}-pubkey==`,
    endpoints: [`203.0.113.${ip}:51820`],
    subnet: `10.210.${ip}.0/24`,
    meshIp: `10.210.${ip}.1`,
    labels: { region: ip === 1 ? 'eu-west' : 'eu-central', zone: `z${ip}` },
    capacity: { cpuMillis: 4000n, memoryBytes: 16n * GiB, diskBytes: 200n * GiB },
    used: { cpuMillis: BigInt(used[0]), memoryBytes: BigInt(used[1]) * GiB, diskBytes: 60n * GiB },
    roles,
    state,
    lastSeen: ago(state === MachineState.DOWN ? 400 : 2),
    createdAt: ago(86400 * 14),
    apiPort: 8443,
    agentVersion: extra.agentVersion ?? 'v0.3.1',
    arch: 'amd64',
    cordoned: extra.cordoned,
  })
}

function svc(id: string, name: string, spec: Service['spec'], active: string): Service {
  return create(ServiceSchema, {
    id,
    projectId: 'prj_shop',
    environmentId: 'env_shop_prod',
    name,
    spec,
    activeDeploymentId: active,
    createdAt: ago(86400 * 9),
    updatedAt: ago(3600),
  })
}

const gh = (repo: string) => ({ github: { repo, branch: 'main', installationId: 1n, prPreviews: false } })

export function seed(): MockDB {
  const machines = [
    machine('mch_nyc01', 'node-1', 1, MachineState.UP, [MachineRole.INGRESS, MachineRole.BUILDER, MachineRole.WORKER], [1800, 7], { agentVersion: 'v0.3.1' }),
    machine('mch_fra02', 'node-2', 2, MachineState.UP, [MachineRole.WORKER, MachineRole.BUILDER], [900, 5], { agentVersion: 'v0.3.0' }),
    machine('mch_ams03', 'node-3', 3, MachineState.DOWN, [MachineRole.WORKER], [0, 0], { cordoned: true, agentVersion: 'v0.3.0' }),
  ]
  const projects = [
    create(ProjectSchema, { id: 'prj_shop', name: 'shop', description: 'Storefront, API and data stores', createdAt: ago(86400 * 9) }),
    create(ProjectSchema, { id: 'prj_blog', name: 'blog', description: 'Static blog with comments', createdAt: ago(86400 * 3) }),
  ]
  const environments = [
    create(EnvironmentSchema, { id: 'env_shop_prod', projectId: 'prj_shop', name: 'default', createdAt: ago(86400 * 9) }),
    create(EnvironmentSchema, { id: 'env_blog_prod', projectId: 'prj_blog', name: 'default', createdAt: ago(86400 * 3) }),
  ]
  const common = { mode: ServiceMode.REPLICATED, strategy: DeployStrategy.START_FIRST, maxSurge: 1, stopGraceSeconds: 10, restartPolicy: 'always' }
  const services = [
    svc('svc_web', 'web', {
      ...common,
      source: gh('acme/shop-web'),
      image: 'registry.internal/shop/web@sha256:3f1c9a',
      replicas: 2,
      ports: [{ containerPort: 3000, protocol: PortProtocol.TCP, mode: 'ingress' }],
      healthCheck: { httpPath: '/healthz', httpPort: 3000, intervalSeconds: 10, timeoutSeconds: 3, retries: 3 },
      resources: { cpuMillis: 500n, memoryBytes: 512n * 1024n * 1024n },
      build: { builder: 'railpack', context: '', watchPaths: ['apps/web/**'], platform: 'linux/amd64' },
      reschedulable: true,
    } as never, 'dep_web_3'),
    svc('svc_api', 'api', {
      ...common,
      source: gh('acme/shop-api'),
      image: 'registry.internal/shop/api@sha256:8be201',
      replicas: 2,
      ports: [{ containerPort: 8080, protocol: PortProtocol.TCP, mode: 'ingress' }],
      healthCheck: { httpPath: '/ready', httpPort: 8080, intervalSeconds: 10, timeoutSeconds: 3, retries: 3 },
      resources: { cpuMillis: 1000n, memoryBytes: 1024n * 1024n * 1024n },
      build: { builder: 'dockerfile', dockerfile: 'Dockerfile', context: '' },
      reschedulable: true,
    } as never, 'dep_api_2'),
    svc('svc_worker', 'worker', {
      ...common,
      source: gh('acme/shop-worker'),
      image: 'registry.internal/shop/worker@sha256:aa91c4',
      replicas: 1,
      build: { builder: 'railpack', context: '', startCommand: 'node dist/worker.js' },
      reschedulable: true,
    } as never, 'dep_worker_1'),
    svc('svc_pg', 'postgres', {
      ...common,
      source: { image: 'postgres:16' },
      image: 'postgres:16',
      replicas: 1,
      strategy: DeployStrategy.STOP_FIRST,
      ports: [{ containerPort: 5432, protocol: PortProtocol.TCP, mode: 'host' }],
      volumes: [{ source: 'pgdata', target: '/var/lib/postgresql/data', persistence: 'backup-restore', sizeBytes: 10n * 1024n * 1024n * 1024n }],
      resources: { cpuMillis: 2000n, memoryBytes: 2048n * 1024n * 1024n },
      reschedulable: false,
    } as never, 'dep_pg_1'),
    svc('svc_redis', 'redis', {
      ...common,
      source: { image: 'redis:7' },
      image: 'redis:7',
      replicas: 1,
      reschedulable: true,
    } as never, 'dep_redis_1'),
  ]
  const v = (n: number, serviceId: string, key: string, value: string, secret = false): Variable =>
    create(VariableSchema, { id: `var_${n}`, environmentId: 'env_shop_prod', serviceId, key, value: secret ? '' : value, secret, redacted: secret })
  const variables = [
    v(1, 'svc_api', 'DATABASE_URL', '${{postgres.DATABASE_URL}}'),
    v(2, 'svc_api', 'REDIS_URL', '${{redis.REDIS_URL}}'),
    v(3, 'svc_api', 'JWT_SECRET', 's3cret', true),
    v(4, 'svc_web', 'API_URL', 'https://${{api.PUBLIC_DOMAIN}}'),
    v(5, 'svc_web', 'NODE_ENV', 'production'),
    v(6, 'svc_worker', 'DATABASE_URL', '${{postgres.DATABASE_URL}}'),
    v(7, 'svc_worker', 'REDIS_URL', '${{redis.REDIS_URL}}'),
    v(8, 'svc_pg', 'DATABASE_URL', 'postgres://shop:${{postgres.POSTGRES_PASSWORD}}@postgres.internal:5432/shop'),
    v(9, 'svc_pg', 'POSTGRES_PASSWORD', 'hunter2', true),
    v(10, 'svc_redis', 'REDIS_URL', 'redis://redis.internal:6379'),
  ]
  const dep = (id: string, serviceId: string, status: DeploymentStatus, ageSec: number, sha: string, msg: string, extra: { previousDeploymentId?: string; buildId?: string; statusMessage?: string } = {}) =>
    create(DeploymentSchema, {
      id,
      serviceId,
      status,
      commitSha: sha,
      commitMessage: msg,
      author: 'alex',
      imageDigest: `sha256:${sha.padEnd(12, '0')}`,
      createdAt: ago(ageSec),
      finishedAt: status === DeploymentStatus.BUILDING || status === DeploymentStatus.DEPLOYING ? undefined : ago(ageSec - 90),
      ...extra,
    })
  const deployments = [
    dep('dep_web_3', 'svc_web', DeploymentStatus.ACTIVE, 1800, '9d3e1ab', 'Add product search', { previousDeploymentId: 'dep_web_2' }),
    dep('dep_web_2', 'svc_web', DeploymentStatus.SUPERSEDED, 86400, 'c04f7e2', 'Fix cart total rounding'),
    dep('dep_web_1', 'svc_web', DeploymentStatus.SUPERSEDED, 86400 * 4, '71aa0d9', 'Initial storefront'),
    dep('dep_api_3', 'svc_api', DeploymentStatus.BUILDING, 45, 'e7b2c11', 'Add order webhooks', { buildId: 'bld_api_3' }),
    dep('dep_api_2', 'svc_api', DeploymentStatus.ACTIVE, 7200, '5be8a30', 'Paginate orders'),
    dep('dep_api_1', 'svc_api', DeploymentStatus.FAILED, 90000, '0a1b2c3', 'Switch to pgx', { statusMessage: 'health check failed on node-2; rolled back' }),
    dep('dep_worker_1', 'svc_worker', DeploymentStatus.ACTIVE, 86400 * 2, 'ff10c8d', 'Email digest job'),
    dep('dep_pg_1', 'svc_pg', DeploymentStatus.ACTIVE, 86400 * 9, 'image', 'postgres:16'),
    dep('dep_redis_1', 'svc_redis', DeploymentStatus.ACTIVE, 86400 * 9, 'image', 'redis:7'),
  ]
  const rep = (id: string, s: string, d: string, m: string, state: ReplicaState, ip: number, msg = ''): Replica =>
    create(ReplicaSchema, { id, serviceId: s, deploymentId: d, machineId: m, containerId: `c${id}`, containerIp: `10.210.${ip}.${10 + Math.floor(Math.random() * 40)}`, state, message: msg, desiredRunning: true, fence: 3n, updatedAt: ago(30) })
  const replicas = [
    rep('rep_web_a', 'svc_web', 'dep_web_3', 'mch_nyc01', ReplicaState.RUNNING, 1),
    rep('rep_web_b', 'svc_web', 'dep_web_3', 'mch_fra02', ReplicaState.RUNNING, 2),
    rep('rep_api_a', 'svc_api', 'dep_api_2', 'mch_nyc01', ReplicaState.RUNNING, 1),
    rep('rep_api_b', 'svc_api', 'dep_api_2', 'mch_fra02', ReplicaState.RUNNING, 2),
    rep('rep_worker_a', 'svc_worker', 'dep_worker_1', 'mch_fra02', ReplicaState.RUNNING, 2),
    rep('rep_pg_a', 'svc_pg', 'dep_pg_1', 'mch_ams03', ReplicaState.STRANDED, 3, 'owner machine down; local volume is not reschedulable'),
    rep('rep_redis_a', 'svc_redis', 'dep_redis_1', 'mch_nyc01', ReplicaState.RUNNING, 1),
  ]
  const dom = (id: string, serviceId: string, hostname: string, port: number, generated: boolean, tls = 'issued'): Domain =>
    create(DomainSchema, { id, serviceId, hostname, port, generated, tlsState: tls, tlsExpiresAt: timestampFromDate(new Date(Date.now() + 86400000 * 60)) })
  const domains = [
    dom('dom_1', 'svc_web', 'web-shop.203-0-113-1.sslip.io', 3000, true),
    dom('dom_2', 'svc_web', 'shop.example.com', 3000, false),
    dom('dom_3', 'svc_api', 'api-shop.203-0-113-1.sslip.io', 8080, true),
    dom('dom_4', 'svc_api', 'api.example.com', 8080, false, 'pending'),
  ]
  const ev = (n: number, type: string, subject: string, message: string, ageSec: number) =>
    create(EventSchema, { id: `evt_${n}`, type, subjectId: subject, message, time: ago(ageSec), attributes: {} })
  const events = [
    ev(1, 'deployment.status', 'dep_api_3', 'api: build started for e7b2c11 “Add order webhooks”', 45),
    ev(2, 'deployment.status', 'dep_web_3', 'web: deployment 9d3e1ab is now active', 1700),
    ev(3, 'machine.state', 'mch_ams03', 'node-3 marked down after 60s without heartbeat', 400),
    ev(4, 'replica.state', 'rep_pg_a', 'postgres replica stranded on node-3 (local volume)', 380),
    ev(5, 'machine.cordon', 'mch_ams03', 'node-3 cordoned', 350),
    ev(6, 'deployment.status', 'dep_api_1', 'api: health check failed, rolled back to 5be8a30', 90000),
    ev(7, 'machine.join', 'mch_fra02', 'node-2 joined the cluster', 86400 * 12),
  ]
  const users = [
    create(UserSchema, { id: 'usr_1', email: 'admin@example.com', name: 'Admin', role: Role.OWNER, createdAt: ago(86400 * 14) }),
    create(UserSchema, { id: 'usr_2', email: 'alex@example.com', name: 'Alex', role: Role.DEVELOPER, createdAt: ago(86400 * 6) }),
    create(UserSchema, { id: 'usr_3', email: 'sam@example.com', name: 'Sam', role: Role.VIEWER, createdAt: ago(86400 * 2) }),
  ]
  const tokens = [
    create(ApiTokenSchema, { id: 'tok_1', name: 'ci-deploy', scopes: ['read', 'deploy'], userId: 'usr_1', createdAt: ago(86400 * 5), lastUsedAt: ago(600) }),
    create(ApiTokenSchema, { id: 'tok_2', name: 'cursor-mcp', scopes: ['read', 'deploy', 'admin'], userId: 'usr_1', createdAt: ago(86400), expiresAt: timestampFromDate(new Date(Date.now() + 86400000 * 29)) }),
  ]
  return {
    cluster: create(ClusterSchema, { id: 'cls_demo', name: 'aetherix-demo', network: '10.210.0.0/16', domain: 'demo.aetherix.cloud', createdAt: ago(86400 * 14) }),
    machines,
    projects,
    environments,
    services,
    variables,
    deployments,
    replicas,
    domains,
    events,
    users,
    tokens,
    github: { configured: false, appId: 0n, slug: '', htmlUrl: '', webhookUrl: '' },
    grants: [
      create(OAuthGrantSchema, { id: 'grt_1', clientId: 'cl_chatgpt', clientName: 'ChatGPT', userId: 'usr_1', scopes: ['read', 'deploy'], createdAt: ago(86400 * 2), lastUsedAt: ago(900) }),
      create(OAuthGrantSchema, { id: 'grt_2', clientId: 'cl_cursor', clientName: 'cursor-vscode', userId: 'usr_1', scopes: ['read'], createdAt: ago(86400 * 6) }),
    ],
    audit: [
      create(AuditEntrySchema, {
        id: 'aud_1', time: ago(90), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'dashboard',
        action: '/aetherix.v1.AuthService/UpdateUser', summary: 'changed role for alex@example.com', ok: true,
        attributes: { user_id: 'usr_2', role: '2' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_2', time: ago(400), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'dashboard',
        action: '/aetherix.v1.MachineService/CordonMachine', summary: 'cordoned node-3', ok: true,
        attributes: { machine: 'mch_ams03' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_3', time: ago(900), actorEmail: 'alex@example.com', actorUserId: 'usr_2', via: 'dashboard',
        action: '/aetherix.v1.DeployService/Rollback', summary: 'rollback web to c04f7e2', ok: true,
        attributes: { service_id: 'svc_web', deployment_id: 'dep_web_2' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_4', time: ago(1800), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'mcp', client: 'ChatGPT',
        action: 'mcp.deploy_service', summary: 'deploy_service api', ok: true,
        attributes: { service: 'api' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_5', time: ago(3600), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'mcp', client: 'cursor-vscode',
        action: 'mcp.delete_service', summary: 'delete_service redis (confirmation required)', ok: false, error: 'confirm: true is required',
        attributes: { service: 'redis' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_6', time: ago(7200), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'dashboard',
        action: 'oauth.consent', summary: 'granted read deploy to ChatGPT', ok: true,
        attributes: { client_name: 'ChatGPT' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_7', time: ago(10800), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'api', credentialId: 'tok_1',
        action: '/aetherix.v1.ClusterService/StartUpgrade', summary: 'upgrade to v0.4.0', ok: true,
        attributes: { version: 'v0.4.0' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_8', time: ago(86400), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'dashboard',
        action: '/aetherix.v1.AuthService/CreateUser', summary: 'invited sam@example.com as viewer', ok: true,
        attributes: { email: 'sam@example.com', role: '1' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_9', time: ago(90000), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'dashboard',
        action: '/aetherix.v1.MachineService/CreateJoinToken', summary: 'join token for node-3', ok: true,
        attributes: { machine_name: 'node-3' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_10', time: ago(95000), actorEmail: 'alex@example.com', actorUserId: 'usr_2', via: 'dashboard',
        action: 'auth.login', summary: 'signed in', ok: true,
      }),
      create(AuditEntrySchema, {
        id: 'aud_11', time: ago(172800), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'dashboard',
        action: '/aetherix.v1.ServiceService/DeleteService', summary: 'deleted staging-worker', ok: true,
        attributes: { service: 'svc_staging_worker' },
      }),
      create(AuditEntrySchema, {
        id: 'aud_12', time: ago(200000), actorEmail: 'admin@example.com', actorUserId: 'usr_1', via: 'api', credentialId: 'tok_1',
        action: '/aetherix.v1.ClusterService/Restore', summary: 'restored cluster snapshot', ok: false, error: 'permission_denied',
        attributes: { name: 'pre-upgrade.snapshot' },
      }),
    ],
    needsSetup: false,
    seq: 100,
  }
}
