import { describe, expect, it } from 'vitest'
import { create } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { DeploymentSchema } from '@/gen/aetherix/v1/types_pb'
import { sortDeploymentsNewestFirst } from './deployments'

describe('sortDeploymentsNewestFirst', () => {
  it('orders by created_at descending even when ids are reverse', () => {
    const older = create(DeploymentSchema, {
      id: 'z-older',
      serviceId: 'svc',
      createdAt: timestampFromDate(new Date(Date.now() - 86400_000)),
    })
    const newer = create(DeploymentSchema, {
      id: 'a-newer',
      serviceId: 'svc',
      createdAt: timestampFromDate(new Date(Date.now() - 180_000)),
    })
    expect(sortDeploymentsNewestFirst([older, newer]).map((d) => d.id)).toEqual(['a-newer', 'z-older'])
  })
})
