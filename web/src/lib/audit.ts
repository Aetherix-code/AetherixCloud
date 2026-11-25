import type { AuditEntry } from '@/gen/aetherix/v1/auth_pb'

/** Human-readable labels for known security-relevant actions. */
const ACTION_LABELS: Record<string, string> = {
  'auth.login': 'Signed in',
  'auth.bootstrap': 'Bootstrapped cluster',
  'oauth.consent': 'Granted OAuth consent',
  'oauth.refresh_reuse': 'Detected refresh-token reuse',
  'AuthService/CreateUser': 'Created user',
  'AuthService/UpdateUser': 'Updated user',
  'AuthService/DeleteUser': 'Deleted user',
  'AuthService/CreateApiToken': 'Created API token',
  'AuthService/RevokeApiToken': 'Revoked API token',
  'AuthService/RevokeOAuthGrant': 'Revoked OAuth grant',
  'MachineService/CreateJoinToken': 'Created join token',
  'MachineService/UpdateMachine': 'Updated machine',
  'MachineService/CordonMachine': 'Cordoned machine',
  'MachineService/DrainMachine': 'Drained machine',
  'MachineService/RemoveMachine': 'Removed machine',
  'ClusterService/UpdateCluster': 'Updated cluster settings',
  'ClusterService/StartUpgrade': 'Started cluster upgrade',
  'ClusterService/CancelUpgrade': 'Cancelled cluster upgrade',
  'ClusterService/Backup': 'Exported cluster backup',
  'ClusterService/Restore': 'Restored cluster snapshot',
  'ProjectService/CreateProject': 'Created project',
  'ProjectService/DeleteProject': 'Deleted project',
  'ProjectService/CreateEnvironment': 'Created environment',
  'ProjectService/DeleteEnvironment': 'Deleted environment',
  'ServiceService/CreateService': 'Created service',
  'ServiceService/UpdateService': 'Updated service',
  'ServiceService/DeleteService': 'Deleted service',
  'DeployService/Deploy': 'Deployed service',
  'DeployService/Rollback': 'Rolled back deployment',
  'DeployService/CancelDeployment': 'Cancelled deployment',
  'GitHubService/BeginManifestSetup': 'Started GitHub App setup',
  'GitHubService/CompleteManifestSetup': 'Completed GitHub App setup',
  'GitHubService/SetWebhookUrl': 'Updated GitHub webhook',
  'GitHubService/SimulatePush': 'Simulated GitHub push',
  deploy_service: 'Deployed service (MCP)',
  delete_service: 'Deleted service (MCP)',
  create_user: 'Created user (MCP)',
  update_user: 'Updated user (MCP)',
  delete_user: 'Deleted user (MCP)',
  cordon_machine: 'Cordoned machine (MCP)',
  remove_machine: 'Removed machine (MCP)',
  start_upgrade: 'Started upgrade (MCP)',
}

const TARGET_KEYS = [
  'user_id',
  'email',
  'machine',
  'machine_id',
  'machine_name',
  'project',
  'project_id',
  'service',
  'service_id',
  'deployment_id',
  'name',
  'version',
  'grant_id',
  'token_id',
  'client_name',
  'role',
]

/** Strip Connect / MCP prefixes into a short procedure name. */
export function shortAction(action: string): string {
  const mcp = action.match(/^mcp\.(.+)$/)
  if (mcp) return mcp[1]
  const rpc = action.match(/^\/?aetherix\.v1\.(.+)$/)
  if (rpc) return rpc[1]
  return action
}

export function auditActionLabel(action: string): string {
  if (ACTION_LABELS[action]) return ACTION_LABELS[action]
  const short = shortAction(action)
  if (ACTION_LABELS[short]) return ACTION_LABELS[short]
  // /aetherix.v1.AuthService/UpdateUser → AuthService/UpdateUser
  if (ACTION_LABELS[short.replace(/^\/?/, '')]) return ACTION_LABELS[short.replace(/^\/?/, '')]
  const method = short.includes('/') ? short.slice(short.lastIndexOf('/') + 1) : short
  return method
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .replace(/_/g, ' ')
    .replace(/^\w/, (c) => c.toUpperCase())
}

export function auditTarget(e: AuditEntry): string {
  const attrs = e.attributes ?? {}
  for (const k of TARGET_KEYS) {
    if (attrs[k]) {
      if (k === 'role') return `role → ${attrs[k]}`
      return attrs[k]
    }
  }
  if (e.summary) {
    // Prefer a concise trailing resource from summary when attrs are empty.
    const m = e.summary.match(/(?:to|on|for|of)\s+([^\s,]+(?:\s+[^\s,]+)?)\s*$/i)
    if (m) return m[1]
    return e.summary.length > 48 ? `${e.summary.slice(0, 48)}…` : e.summary
  }
  return '—'
}

export type AuditCategory = 'all' | 'auth' | 'cluster' | 'deploy' | 'oauth' | 'failed'

export function auditCategory(action: string): Exclude<AuditCategory, 'all' | 'failed'> {
  const a = action.toLowerCase()
  const tool = a.startsWith('mcp.') ? a.slice(4) : a
  if (
    a.startsWith('auth.') ||
    a.includes('authservice/') ||
    tool.includes('create_user') ||
    tool.includes('update_user') ||
    tool.includes('delete_user')
  ) {
    return 'auth'
  }
  if (a.startsWith('oauth.') || a.includes('apitoken') || a.includes('oauthgrant') || tool.includes('token') || tool.includes('grant')) {
    return 'oauth'
  }
  if (
    a.includes('machineservice/') ||
    a.includes('clusterservice/') ||
    tool.includes('machine') ||
    tool.includes('upgrade') ||
    tool.includes('backup') ||
    tool.includes('restore') ||
    tool.includes('cordon') ||
    tool.includes('drain')
  ) {
    return 'cluster'
  }
  return 'deploy'
}

export function matchesAuditFilter(e: AuditEntry, category: AuditCategory, actor: string, q: string): boolean {
  if (category === 'failed' && e.ok) return false
  if (category !== 'all' && category !== 'failed' && auditCategory(e.action) !== category) return false
  if (actor && e.actorUserId !== actor && e.actorEmail !== actor) return false
  if (q) {
    const hay = `${e.actorEmail} ${e.actorUserId} ${e.action} ${e.summary} ${auditTarget(e)} ${e.via} ${e.client}`.toLowerCase()
    if (!hay.includes(q.toLowerCase())) return false
  }
  return true
}
