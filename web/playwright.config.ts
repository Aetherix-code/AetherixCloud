import { defineConfig, devices } from '@playwright/test'

const port = Number(process.env.AX_E2E_PORT ?? 5173)
const baseURL = process.env.AX_E2E_URL ?? `http://127.0.0.1:${port}`

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  timeout: 60_000,
  expect: { timeout: 15_000 },
  use: {
    baseURL,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    ...devices['Desktop Chrome'],
  },
  // Default suite is mock-mode. Set AX_E2E_URL to hit a real `ax dev` cluster instead.
  webServer: process.env.AX_E2E_URL
    ? undefined
    : {
        command: `npm run dev -- --host 127.0.0.1 --port ${port}`,
        url: baseURL,
        reuseExistingServer: !process.env.CI,
        env: { ...process.env, VITE_MOCK: '1' },
        timeout: 120_000,
      },
  projects: [
    { name: 'mock', testMatch: /.*\.spec\.ts/, testIgnore: /.*\.real\.spec\.ts/ },
    { name: 'real', testMatch: /.*\.real\.spec\.ts/, grep: process.env.AX_E2E_URL ? /.*/ : /a^/ },
  ],
})
