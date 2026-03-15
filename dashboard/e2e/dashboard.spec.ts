import { test, expect } from '@playwright/test'

// Smoke test — verifies the app renders at all.
// Replace with real assertions once the dashboard is implemented.
test('loads the home page', async ({ page }) => {
  await page.goto('/')
  await expect(page).toHaveTitle(/Monitower|Next/)
})
