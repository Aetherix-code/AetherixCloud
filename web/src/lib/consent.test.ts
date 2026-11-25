import { afterEach, describe, expect, it, vi } from 'vitest'
import { tokenStore } from './api'
import { fetchConsent, submitConsent } from './consent'
import { isStreamEvent } from './queries'

afterEach(() => {
  vi.unstubAllGlobals()
  localStorage.clear()
})

describe('oauth consent client', () => {
  it('sends the session JWT and returns redirect_url', async () => {
    tokenStore.set('jwt-123')
    const fetchMock = vi.fn(async () => new Response(JSON.stringify({ redirect_url: 'https://x.test/cb?code=1' }), { status: 200 }))
    vi.stubGlobal('fetch', fetchMock)
    expect(await submitConsent('req1', true, ['read'])).toBe('https://x.test/cb?code=1')
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toContain('/oauth/api/consent')
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer jwt-123')
    expect(JSON.parse(init.body as string)).toEqual({ request: 'req1', approve: true, scopes: ['read'] })
  })
  it('surfaces OAuth errors', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ error: 'login_required', error_description: 'a dashboard session is required' }), { status: 401 })))
    await expect(fetchConsent('x')).rejects.toThrow('a dashboard session is required')
  })
})

describe('events', () => {
  it('ignores stream.* events', () => {
    expect(isStreamEvent('stream.logs.open')).toBe(true)
    expect(isStreamEvent('deployment.status')).toBe(false)
  })
})
