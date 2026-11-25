import { expect, test } from '@playwright/test'

/**
 * Optional real-backend smoke. Only runs when AX_E2E_URL is set (see playwright.config.ts).
 * Example: AX_E2E_URL=https://127.0.0.1:8443 npm run test:e2e -- --project=real
 */
test.describe('real backend smoke', () => {
  test.skip(!process.env.AX_E2E_URL, 'AX_E2E_URL not set')

  test('login page renders against live cluster', async ({ page }) => {
    await page.goto('/login')
    await expect(page.getByText('AetherixCloud')).toBeVisible()
    await expect(page.getByRole('button', { name: /sign in/i })).toBeVisible()
  })
})
