import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import type { User } from '@/gen/aetherix/v1/types_pb'
import { api, isUnauthenticated, isUnimplemented, tokenStore } from './api'

type Status = 'loading' | 'authed' | 'anon'
interface AuthCtx {
  status: Status
  user?: User
  scopes: string[]
  /** True when AuthService answered Unimplemented: the node trusts the network (mesh/loopback). */
  authDisabled: boolean
  login(email: string, password: string): Promise<void>
  bootstrap(a: { email: string; name: string; password: string; adminToken: string }): Promise<void>
  loginWithToken(token: string): Promise<void>
  logout(): Promise<void>
}
const Ctx = createContext<AuthCtx | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<Status>('loading')
  const [user, setUser] = useState<User>()
  const [scopes, setScopes] = useState<string[]>([])
  const [authDisabled, setAuthDisabled] = useState(false)

  const refresh = useCallback(async () => {
    try {
      const me = await api.auth.me({})
      setUser(me.user)
      setScopes(me.scopes)
      setAuthDisabled(false)
      setStatus('authed')
    } catch (e) {
      if (isUnimplemented(e)) {
        // AuthService not shipped: probe an authenticated read to learn whether a token is required.
        try {
          await api.cluster.getCluster({})
          setAuthDisabled(true)
          setStatus('authed')
        } catch (e2) {
          setStatus(isUnauthenticated(e2) ? 'anon' : 'authed')
          setAuthDisabled(true)
        }
      } else if (isUnauthenticated(e)) setStatus('anon')
      else setStatus('anon')
    }
  }, [])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const value = useMemo<AuthCtx>(
    () => ({
      status,
      user,
      scopes,
      authDisabled,
      async login(email, password) {
        const r = await api.auth.login({ email, password })
        tokenStore.set(r.sessionToken)
        await refresh()
      },
      async bootstrap(a) {
        const r = await api.auth.bootstrap(a)
        tokenStore.set(r.sessionToken)
        await refresh()
      },
      async loginWithToken(token) {
        tokenStore.set(token)
        try {
          await api.cluster.getCluster({})
        } catch (e) {
          tokenStore.clear()
          throw e
        }
        await refresh()
      },
      async logout() {
        try {
          await api.auth.logout({})
        } catch {
          /* best effort */
        }
        tokenStore.clear()
        setUser(undefined)
        setStatus('anon')
      },
    }),
    [status, user, scopes, authDisabled, refresh],
  )
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useAuth(): AuthCtx {
  const c = useContext(Ctx)
  if (!c) throw new Error('AuthProvider missing')
  return c
}
