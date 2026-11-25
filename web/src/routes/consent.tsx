import { useEffect, useState } from 'react'
import { Navigate } from '@tanstack/react-router'
import { Bot, ShieldCheck } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { CheckboxField } from '@/components/ui/checkbox'
import { Skeleton } from '@/components/ui/skeleton'
import { useAuth } from '@/lib/auth'
import { fetchConsent, submitConsent, type ConsentRequest } from '@/lib/consent'

const scopeHelp: Record<string, string> = {
  read: 'View projects, services, deployments, logs and metrics (secret values stay hidden)',
  deploy: 'Create, deploy, scale and roll back services; change variables and domains',
  admin: 'Manage machines, delete services and projects, and issue join commands',
}

/** OAuth 2.1 consent screen for MCP connectors (ChatGPT, Claude, Cursor). */
export function ConsentPage() {
  const { status } = useAuth()
  const request = new URLSearchParams(window.location.search).get('request') ?? ''
  const [info, setInfo] = useState<ConsentRequest>()
  const [scopes, setScopes] = useState<string[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (status !== 'authed' || !request) return
    fetchConsent(request)
      .then((c) => {
        setInfo(c)
        setScopes(c.requested_scopes.filter((s) => c.allowed_scopes.includes(s)))
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
  }, [status, request])

  if (status === 'loading') return <div className="p-8"><Skeleton className="h-8 w-48" /></div>
  if (status === 'anon') {
    const back = `${window.location.pathname}${window.location.search}`
    return <Navigate to="/login" search={{ redirect: back }} />
  }

  async function decide(approve: boolean) {
    setBusy(true)
    setError('')
    try {
      const url = await submitConsent(request, approve, scopes)
      window.location.assign(url) // external redirect back to the connector
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setBusy(false)
    }
  }

  return (
    <div className="dot-grid grid min-h-full place-items-center p-4">
      <Card className="w-full max-w-md"><CardContent className="space-y-4">
        {!request ? (
          <p role="alert" className="text-sm text-destructive">Missing authorization request.</p>
        ) : !info ? (
          error ? <p role="alert" className="text-sm text-destructive">{error}</p> : <Skeleton className="h-40 w-full" />
        ) : (
          <>
            <div className="flex items-center gap-3">
              <div className="grid size-10 place-items-center rounded-lg bg-accent"><Bot className="size-5 text-primary" /></div>
              <div>
                <h1 className="text-base font-semibold">Authorize {info.client_name || info.client_id}</h1>
                <p className="text-xs text-muted-foreground">will act as {info.user.email}, redirecting to {info.redirect_scheme}://{info.redirect_host}</p>
              </div>
            </div>
            <fieldset className="space-y-2">
              <legend className="mb-1 text-xs font-medium text-muted-foreground">Requested access</legend>
              {info.requested_scopes.map((s) => {
                const allowed = info.allowed_scopes.includes(s)
                return (
                  <div key={s} className={`rounded-md border p-2.5 ${allowed ? '' : 'opacity-50'}`}>
                    <CheckboxField
                      disabled={!allowed}
                      checked={scopes.includes(s)}
                      onCheckedChange={(on) => setScopes(on ? [...scopes, s] : scopes.filter((x) => x !== s))}
                    >
                      <span className="font-medium">{s}</span>
                      {!allowed && <span className="text-xs"> (your role doesn’t allow this)</span>}
                      <span className="mt-0.5 block text-xs text-muted-foreground">{scopeHelp[s] ?? ''}</span>
                    </CheckboxField>
                  </div>
                )
              })}
            </fieldset>
            <p className="flex items-start gap-2 text-xs text-muted-foreground"><ShieldCheck className="mt-0.5 size-3.5 shrink-0" />You can revoke this connector any time under Settings → AI agents. Destructive tools still require confirmation.</p>
            {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" onClick={() => decide(false)} disabled={busy}>Deny</Button>
              <Button onClick={() => decide(true)} disabled={busy || !scopes.length}>Approve</Button>
            </div>
          </>
        )}
      </CardContent></Card>
    </div>
  )
}
