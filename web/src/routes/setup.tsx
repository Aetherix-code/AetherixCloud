import { useState } from 'react'
import { Navigate, useNavigate } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Field, Input } from '@/components/ui/input'
import { errMessage } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { useSetupStatus } from '@/lib/queries'

/** First-run screen: creates the owner account (AuthService.Bootstrap, public until a user exists). */
export function SetupPage() {
  const { bootstrap } = useAuth()
  const status = useSetupStatus()
  const nav = useNavigate()
  const [f, setF] = useState({ email: '', name: '', password: '', confirm: '', adminToken: '' })
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  if (status.isSuccess && !status.data) return <Navigate to="/login" search={{}} />
  const mismatch = f.confirm !== '' && f.confirm !== f.password

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (f.password !== f.confirm) return setError('Passwords do not match')
    setBusy(true)
    setError('')
    try {
      await bootstrap({ email: f.email, name: f.name, password: f.password, adminToken: f.adminToken })
      await nav({ to: '/' })
    } catch (err) {
      setError(errMessage(err))
    } finally {
      setBusy(false)
    }
  }
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })

  return (
    <div className="dot-grid grid min-h-full place-items-center p-4">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex items-center justify-center gap-2">
          <div className="grid size-9 place-items-center rounded-lg bg-primary text-lg font-bold text-primary-foreground">A</div>
          <span className="text-lg font-semibold">AetherixCloud</span>
        </div>
        <Card><CardContent>
          <form onSubmit={submit} className="space-y-4">
            <div>
              <h1 className="text-base font-semibold">Welcome — create your owner account</h1>
              <p className="mt-1 text-xs text-muted-foreground">This cluster has no users yet. The first account becomes the owner.</p>
            </div>
            <Field label="Name"><Input value={f.name} onChange={set('name')} autoFocus /></Field>
            <Field label="Email"><Input type="email" required autoComplete="username" value={f.email} onChange={set('email')} /></Field>
            <Field label="Password" hint="At least 8 characters."><Input type="password" required minLength={8} autoComplete="new-password" value={f.password} onChange={set('password')} /></Field>
            <Field label="Confirm password"><Input type="password" required autoComplete="new-password" value={f.confirm} onChange={set('confirm')} aria-invalid={mismatch} /></Field>
            <Field label="Setup token (if required)" hint="Printed by `aetherixd init`; only needed when the node was started with an admin token."><Input type="password" value={f.adminToken} onChange={set('adminToken')} /></Field>
            {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={busy || mismatch}>{busy ? 'Creating…' : 'Create owner account'}</Button>
          </form>
        </CardContent></Card>
      </div>
    </div>
  )
}
