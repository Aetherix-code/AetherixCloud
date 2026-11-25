import { apiBase, isMock, tokenStore } from './api'

export interface ConsentRequest {
  client_id: string
  client_name: string
  redirect_host: string
  redirect_scheme: string
  requested_scopes: string[]
  allowed_scopes: string[]
  resource: string
  user: { id: string; email: string }
}

const base = () => `${apiBase || window.location.origin}/oauth/api/consent`
const headers = (): Record<string, string> => {
  const t = tokenStore.get()
  return t ? { Authorization: `Bearer ${t}` } : {}
}

async function errorOf(res: Response): Promise<Error> {
  try {
    const j = await res.json()
    return new Error(j.error_description || j.error || `HTTP ${res.status}`)
  } catch {
    return new Error(`HTTP ${res.status}`)
  }
}

export async function fetchConsent(request: string): Promise<ConsentRequest> {
  if (isMock)
    return { client_id: 'cl_demo', client_name: 'ChatGPT', redirect_host: 'chatgpt.com', redirect_scheme: 'https', requested_scopes: ['read', 'deploy', 'admin'], allowed_scopes: ['read', 'deploy'], resource: '', user: { id: 'usr_1', email: 'admin@example.com' } }
  const res = await fetch(`${base()}?request=${encodeURIComponent(request)}`, { headers: headers() })
  if (!res.ok) throw await errorOf(res)
  return res.json()
}

/** Returns the URL to navigate to (the connector's redirect_uri with code or error). */
export async function submitConsent(request: string, approve: boolean, scopes: string[]): Promise<string> {
  if (isMock) return `${window.location.origin}/settings/agents`
  const res = await fetch(base(), { method: 'POST', headers: { ...headers(), 'Content-Type': 'application/json' }, body: JSON.stringify({ request, approve, scopes }) })
  if (!res.ok) throw await errorOf(res)
  const j = (await res.json()) as { redirect_url: string }
  return j.redirect_url
}
