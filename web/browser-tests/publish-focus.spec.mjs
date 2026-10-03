import { test, expect } from 'playwright/test'

const artifacts = [
  { path: 'xform/plan.html', title: 'Plan', description: '', state: 'published', total_size: 42, updated_at: '2025-01-01', public_url: 'https://pub.bdgn.me/xform/plan.html' },
  { path: 'xform/demo/', title: 'Demo', description: '', state: 'published', total_size: 80, updated_at: '2025-01-02', public_url: 'https://pub.bdgn.me/xform/demo/' },
]

test.beforeEach(async ({ page }) => {
  await page.route('**/ui/api/**', route => {
    const path = new URL(route.request().url()).pathname
    const data = path.endsWith('/artifacts') ? artifacts : path.endsWith('/projects') ? [{ name: 'xform', artifact_count: 2, description: '' }] : path.endsWith('/config') ? { public_base_url: 'https://pub.bdgn.me' } : { label: 'admin@example.test' }
    return route.fulfill({ json: data })
  })
  await page.goto('/')
  await expect(page.getByText('Plan', { exact: true })).toBeVisible()
})

test('new HTML Artifact guides the untouched path and returns focus on Cancel', async ({ page }) => {
  const opener = page.getByRole('button', { name: 'Publish', exact: true }).first()
  await opener.focus()
  await page.keyboard.press('Enter')
  const form = page.locator('.publish-form')
  const path = form.getByRole('textbox', { name: 'Artifact path' })
  await expect(path).toBeFocused()
  await expect(form.getByText(/Project.*\.html|Project.*Bundle/i)).toBeVisible()
  await expect(form.getByRole('alert')).toHaveCount(0)
  await path.fill('xform/Plan.html')
  await expect(form.getByRole('alert')).toContainText('Invalid path segment: Plan')
  await path.fill('')
  await expect(form.getByRole('alert')).toContainText('Use .html for a file or / for a Bundle')
  await form.getByRole('button', { name: 'Cancel' }).click()
  await expect(opener).toBeFocused()
})

test('folder selection yields a Bundle path and timely conflict feedback', async ({ page }) => {
  await page.getByRole('button', { name: 'Publish', exact: true }).first().click()
  const form = page.locator('.publish-form')
  await form.locator('input[webkitdirectory]').setInputFiles('browser-tests/fixtures/sample-bundle')
  const path = form.getByRole('textbox', { name: 'Artifact path' })
  await expect(path).toHaveValue('sample-bundle/')
  await expect(form.getByRole('alert')).toContainText('An Artifact must be below a Project')
  await path.fill('xform/demo/')
  await expect(form.getByText('This will replace the existing Artifact at this URL.')).toBeVisible()
  await expect(form.getByRole('alert')).toHaveCount(0)
})

test('Republish of an HTML Artifact focuses file selection and reports a mismatched folder', async ({ page }) => {
  await page.getByRole('button', { name: 'Publish new version of xform/plan.html' }).click()
  const form = page.locator('.publish-form')
  await expect(form.locator('input[type="file"]').first()).toBeFocused()
  await expect(form.getByRole('alert')).toHaveCount(0)
  await form.locator('input[webkitdirectory]').setInputFiles('browser-tests/fixtures/sample-bundle')
  await expect(form.getByRole('alert')).toContainText('A folder needs a trailing /')
  await expect(form.getByRole('button', { name: 'Publish', exact: true })).toBeDisabled()
})

test('Republish focuses the file choice when the path is fixed and Cancel returns to row', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 })
  const opener = page.getByRole('button', { name: 'Publish new version of xform/demo/' })
  await opener.focus()
  await page.keyboard.press('Enter')
  const form = page.locator('.publish-form')
  await expect(form.locator('input[webkitdirectory]')).toBeFocused()
  const bounds = await form.boundingBox()
  expect(bounds.x).toBeGreaterThanOrEqual(0)
  expect(bounds.x + bounds.width).toBeLessThanOrEqual(390)
  await expect(form.getByRole('textbox', { name: 'Artifact path' })).toHaveValue('xform/demo/')
  await expect(form.getByRole('alert')).toHaveCount(0)
  await form.getByRole('button', { name: 'Cancel' }).click()
  await expect(opener).toBeFocused()
})
