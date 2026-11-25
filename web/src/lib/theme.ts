import { useSyncExternalStore } from 'react'

export type Theme = 'dark' | 'light'
const KEY = 'ax.theme'
const subs = new Set<() => void>()

export function getTheme(): Theme {
  return document.documentElement.classList.contains('dark') ? 'dark' : 'light'
}
export function setTheme(t: Theme) {
  document.documentElement.classList.toggle('dark', t === 'dark')
  try {
    localStorage.setItem(KEY, t)
  } catch {
    /* storage unavailable */
  }
  subs.forEach((f) => f())
}
export function useTheme(): [Theme, () => void] {
  const t = useSyncExternalStore(
    (cb) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    getTheme,
  )
  return [t, () => setTheme(t === 'dark' ? 'light' : 'dark')]
}
