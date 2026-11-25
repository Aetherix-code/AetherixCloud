import { useSyncExternalStore } from 'react'

const KEY = 'ax.sidebarCollapsed'
const subs = new Set<() => void>()

function read(): boolean {
  try {
    return localStorage.getItem(KEY) === '1'
  } catch {
    return false
  }
}

export function getSidebarCollapsed(): boolean {
  return read()
}

export function setSidebarCollapsed(collapsed: boolean) {
  try {
    localStorage.setItem(KEY, collapsed ? '1' : '0')
  } catch {
    /* storage unavailable */
  }
  subs.forEach((f) => f())
}

export function useSidebarCollapsed(): [boolean, () => void] {
  const collapsed = useSyncExternalStore(
    (cb) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    getSidebarCollapsed,
    () => false,
  )
  return [collapsed, () => setSidebarCollapsed(!collapsed)]
}
