import { expect, test, type Page } from '@playwright/test'

/** Mock transport answers Me without a token, so the app boots already authed. */
async function reachProjects(page: Page) {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Projects' })).toBeVisible({ timeout: 20_000 })
  await expect(page.getByText('mock data')).toBeVisible()
}

async function loginViaForm(page: Page) {
  await reachProjects(page)
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
  await page.locator('input[type="email"]').fill('admin@example.com')
  await page.locator('input[type="password"]').fill('demo')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('heading', { name: 'Projects' })).toBeVisible({ timeout: 20_000 })
}

test.describe('dashboard (VITE_MOCK=1)', () => {
  test('login lands on projects', async ({ page }) => {
    await loginViaForm(page)
    await expect(page.getByRole('link', { name: /shop/i })).toBeVisible()
    await expect(page.getByRole('link', { name: /blog/i })).toBeVisible()
  })

  test('project canvas and service panel tabs', async ({ page }) => {
    await reachProjects(page)
    await page.getByRole('link', { name: /shop/i }).click()
    await expect(page.getByTestId('project-canvas')).toBeVisible()
    await expect(page.getByRole('heading', { name: 'shop' })).toBeVisible()

    // Open the web service node on the canvas.
    await page.getByText('web', { exact: true }).first().click()
    const panel = page.getByRole('complementary', { name: /web panel/i })
    await expect(panel).toBeVisible()
    await expect(panel.getByRole('heading', { name: 'web' })).toBeVisible()

    await panel.getByRole('tab', { name: 'Variables' }).click()
    await expect(panel.getByRole('tab', { name: 'Variables' })).toHaveAttribute('data-state', 'active')
    await panel.getByRole('tab', { name: 'Settings' }).click()
    await expect(panel.getByRole('tab', { name: 'Settings' })).toHaveAttribute('data-state', 'active')
    await panel.getByRole('tab', { name: 'Deployments' }).click()
    await expect(panel.getByRole('tab', { name: 'Deployments' })).toHaveAttribute('data-state', 'active')

    await panel.getByRole('button', { name: 'Close panel' }).click()
    await expect(panel).toHaveCount(0)
  })

  test('cluster machine map', async ({ page }) => {
    await reachProjects(page)
    await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Cluster' }).click()
    await expect(page.getByRole('heading', { name: 'Cluster' })).toBeVisible()
    const map = page.getByLabel('Machine map')
    await expect(map).toBeVisible()
    await expect(map.getByRole('link', { name: /node-1/ })).toBeVisible()
    await expect(map.getByRole('link', { name: /node-2/ })).toBeVisible()
    await map.getByRole('link', { name: /node-1/ }).click()
    await expect(page.getByRole('heading', { name: 'node-1' })).toBeVisible()
  })
})
