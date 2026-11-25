import { createClient } from '@connectrpc/connect'
import { describe, expect, it } from 'vitest'
import { DeployService } from '@/gen/aetherix/v1/deploy_pb'
import { MachineService } from '@/gen/aetherix/v1/machine_pb'
import { ProjectService } from '@/gen/aetherix/v1/project_pb'
import { AuthService } from '@/gen/aetherix/v1/auth_pb'
import { DeploymentStatus } from '@/gen/aetherix/v1/types_pb'
import { isUnimplemented } from '@/lib/api'
import { createMockTransport } from './transport'

describe('mock backend', () => {
  const t = createMockTransport()
  it('serves projects with services and variable references', async () => {
    const projects = createClient(ProjectService, t)
    const { projects: list } = await projects.listProjects({})
    expect(list.map((p) => p.name)).toContain('shop')
    const g = await projects.getProject({ project: 'shop' })
    expect(g.services.length).toBeGreaterThanOrEqual(4)
    const vars = await projects.listVariables({ environmentId: 'env_shop_prod', serviceId: '*' })
    expect(vars.variables.some((v) => v.value.includes('${{postgres.'))).toBe(true)
    expect(vars.variables.find((v) => v.key === 'JWT_SECRET')?.redacted).toBe(true)
  })
  it('rollback creates a new deployment pointing at the old digest', async () => {
    const deploy = createClient(DeployService, t)
    const r = await deploy.rollback({ serviceId: 'svc_web', deploymentId: 'dep_web_2' })
    expect(r.deployment?.status).toBe(DeploymentStatus.QUEUED)
    expect(r.deployment?.imageDigest).toContain('c04f7e2')
  })
  it('creates join tokens and drains machines', async () => {
    const m = createClient(MachineService, t)
    expect((await m.createJoinToken({ machineName: 'node-9' })).installCommand).toContain('node-9')
    const d = await m.drainMachine({ machine: 'node-2' })
    expect(d.machine?.cordoned).toBe(true)
  })
  it('errors map to Connect codes', async () => {
    const auth = createClient(AuthService, t)
    await expect(auth.login({ email: 'a@b.c', password: '' })).rejects.toSatisfy((e) => !isUnimplemented(e))
  })
})
