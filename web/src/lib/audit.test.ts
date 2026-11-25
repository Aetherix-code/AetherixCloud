import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { AuditEntrySchema } from '@/gen/aetherix/v1/auth_pb'
import { auditActionLabel, auditCategory, auditTarget, matchesAuditFilter, shortAction } from './audit'

describe('audit helpers', () => {
  it('shortens RPC and MCP action names', () => {
    expect(shortAction('/aetherix.v1.AuthService/UpdateUser')).toBe('AuthService/UpdateUser')
    expect(shortAction('mcp.delete_service')).toBe('delete_service')
  })

  it('labels known security actions', () => {
    expect(auditActionLabel('/aetherix.v1.MachineService/RemoveMachine')).toBe('Removed machine')
    expect(auditActionLabel('auth.login')).toBe('Signed in')
    expect(auditActionLabel('oauth.consent')).toBe('Granted OAuth consent')
  })

  it('picks a target from attributes', () => {
    const e = create(AuditEntrySchema, {
      action: '/aetherix.v1.AuthService/UpdateUser',
      attributes: { user_id: 'usr_2', role: '3' },
    })
    expect(auditTarget(e)).toBe('usr_2')
  })

  it('categorizes and filters entries', () => {
    const e = create(AuditEntrySchema, {
      actorEmail: 'admin@example.com',
      actorUserId: 'usr_1',
      action: '/aetherix.v1.ClusterService/StartUpgrade',
      ok: true,
      summary: 'upgrade to v0.4.0',
    })
    expect(auditCategory(e.action)).toBe('cluster')
    expect(auditCategory('mcp.deploy_service')).toBe('deploy')
    expect(auditCategory('mcp.delete_user')).toBe('auth')
    expect(matchesAuditFilter(e, 'cluster', '', '')).toBe(true)
    expect(matchesAuditFilter(e, 'auth', '', '')).toBe(false)
    expect(matchesAuditFilter(e, 'all', 'usr_1', 'upgrade')).toBe(true)
    expect(matchesAuditFilter(e, 'failed', '', '')).toBe(false)
  })
})
