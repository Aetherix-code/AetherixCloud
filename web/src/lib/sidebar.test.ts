import { afterEach, describe, expect, it } from 'vitest'
import { getSidebarCollapsed, setSidebarCollapsed } from './sidebar'

afterEach(() => {
  localStorage.removeItem('ax.sidebarCollapsed')
})

describe('sidebar collapse preference', () => {
  it('defaults to expanded', () => {
    expect(getSidebarCollapsed()).toBe(false)
  })
  it('persists collapse', () => {
    setSidebarCollapsed(true)
    expect(getSidebarCollapsed()).toBe(true)
    setSidebarCollapsed(false)
    expect(getSidebarCollapsed()).toBe(false)
  })
})
