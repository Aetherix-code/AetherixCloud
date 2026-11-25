import { useState } from 'react'
import { getRouteApi, useNavigate } from '@tanstack/react-router'
import { KeyRound } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Field, Input } from '@/components/ui/input'
import { errMessage, isMock, isUnimplemented } from '@/lib/api'
import { useAuth } from '@/lib/auth'

const route = getRouteApi('/login')

export function LoginPage() {
  const { redirect } = route.useSearch()
  const { login, loginWithToken, status } = useAuth()
  const nav = useNavigate()
  const [mode, setMode] = useState<'password' | 'token'>('password')
  const [email, setEmail] = useState(isMock ? 'admin@example.com' : '')
  const [password, setPassword] = useState(isMock ? 'demo' : '')
  const [token, setToken] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const done = async () => {
    if (redirect && redirect.startsWith('/') && !redirect.startsWith('//')) window.location.assign(redirect)
    else await nav({ to: '/' })
  }
  if (status === 'authed') void done()

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      if (mode === 'password') await login(email, password)
      else await loginWithToken(token.trim())
      await done()
    } catch (err) {
      if (mode === 'password' && isUnimplemented(err)) {
        setError('Password login is not available on this node yet. Sign in with an API token instead.')
        setMode('token')
      } else setError(errMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="dot-grid grid min-h-full place-items-center p-4">
      <div className="ax-enter w-full max-w-sm">
        <div className="mb-7 flex flex-col items-center gap-3 text-center">
          <div className="grid size-11 place-items-center rounded-xl bg-primary text-xl font-bold text-primary-foreground shadow-sm">A</div>
          <div>
            <div className="text-xl font-semibold tracking-tight">AetherixCloud</div>
            <p className="mt-1 text-sm text-muted-foreground">Sign in to your cluster</p>
          </div>
        </div>
        <Card>
          <CardContent className="pt-5">
            <form onSubmit={submit} className="space-y-4">
              <h1 className="text-sm font-semibold tracking-tight">{mode === 'password' ? 'Sign in' : 'Sign in with an API token'}</h1>
              {mode === 'password' ? (
                <>
                  <Field label="Email"><Input type="email" autoComplete="username" value={email} onChange={(e) => setEmail(e.target.value)} required autoFocus /></Field>
                  <Field label="Password"><Input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required /></Field>
                </>
              ) : (
                <Field label="API token" hint="Created with `ax token create` or in Settings → AI agents.">
                  <Input type="password" value={token} onChange={(e) => setToken(e.target.value)} placeholder="axt_…" required autoFocus />
                </Field>
              )}
              {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
              <Button type="submit" className="w-full" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</Button>
              <Button type="button" variant="ghost" className="w-full" onClick={() => { setMode(mode === 'password' ? 'token' : 'password'); setError('') }}>
                <KeyRound />{mode === 'password' ? 'Use an API token' : 'Use email and password'}
              </Button>
            </form>
          </CardContent>
        </Card>
        {isMock && <p className="mt-4 text-center text-xs text-muted-foreground">Mock mode: any password works.</p>}
      </div>
    </div>
  )
}
