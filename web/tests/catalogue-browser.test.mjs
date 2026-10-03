import { test, before, after } from 'node:test'
import { strict as assert } from 'node:assert'
import { createServer } from 'vite'
import { chromium } from 'playwright'

let server
let browser
before(async () => {
  server = await createServer({ server: { host: '127.0.0.1', port: 0 } })
  await server.listen()
  browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) })
})
after(async () => { await browser?.close(); await server?.close() })

const origin = () => server.resolvedUrls.local[0]
async function catalogue(page, artifacts) {
  const projects = [{ name: 'alpha', description: 'Published examples', artifact_count: artifacts.length }]
  await page.route('**/ui/api/**', route => {
    const path = new URL(route.request().url()).pathname
    const data = path.endsWith('/artifacts') ? artifacts : path.endsWith('/projects') ? projects : path.endsWith('/config') ? { public_base_url: 'https://pub.example.test' } : { label: 'owner@example.test' }
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) })
  })
  await page.goto(origin())
  await page.getByRole('button', { name: 'Incomplete 0' }).waitFor()
}
const sample = [{ path: 'alpha/guide.html', title: 'Guide', description: '', state: 'published', total_size: 12, updated_at: '2025-01-01T00:00:00Z', last_publisher: 'owner' }]

// Browser-visible Catalogue seam: safe, read-only API fixtures; no live Portal or Artifact host.
test('empty Incomplete filter explains zero and keyboard Show all restores the list at narrow and wide widths', async () => {
  for (const width of [390, 1200]) {
    const page = await browser.newPage({ viewport: { width, height: 800 } })
    try {
      await catalogue(page, sample)
      await page.getByRole('textbox', { name: 'Search Catalogue' }).focus()
      await page.keyboard.press('Tab')
      await page.keyboard.press('Tab')
      assert.equal(await page.evaluate(() => document.activeElement?.textContent?.trim()), 'Incomplete 0')
      await page.keyboard.press('Enter')
      assert.match(await page.locator('main').innerText(), /No Incomplete Artifacts/)
      assert.equal(await page.getByRole('button', { name: 'Show all' }).count(), 1)
      await page.getByRole('button', { name: 'Show all' }).focus()
      await page.keyboard.press('Enter')
      assert.equal(await page.getByText('Guide', { exact: true }).count(), 1)
      assert.equal(await page.getByRole('button', { name: 'All 1' }).getAttribute('aria-pressed'), 'true')
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    } finally { await page.close() }
  }
})

test('search with no matches offers keyboard-clear search and filters independently', async () => {
  for (const width of [390, 1200]) {
    const page = await browser.newPage({ viewport: { width, height: 800 } })
    try {
      await catalogue(page, sample)
      await page.keyboard.press('/')
      assert.equal(await page.evaluate(() => document.activeElement?.getAttribute('aria-label')), 'Search Catalogue')
      await page.keyboard.type('absent')
      assert.match(await page.locator('main').innerText(), /No results for/)
      await page.getByRole('button', { name: 'Clear search' }).focus()
      await page.keyboard.press('Enter')
      assert.equal(await page.getByText('Guide', { exact: true }).count(), 1)
      await page.getByRole('button', { name: 'Incomplete 0' }).click()
      await page.getByRole('textbox', { name: 'Search Catalogue' }).fill('absent')
      await page.getByRole('button', { name: 'Clear search and show all' }).focus()
      await page.keyboard.press('Enter')
      assert.equal(await page.getByText('Guide', { exact: true }).count(), 1)
      assert.equal(await page.getByRole('textbox', { name: 'Search Catalogue' }).inputValue(), '')
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    } finally { await page.close() }
  }
})

// Resolve translucent backgrounds against the rendered ancestor backgrounds, not a guessed theme color.
async function contrast(locator) {
  return locator.evaluate(element => {
    const rgba = value => (value.match(/[\d.]+/g) || []).map(Number)
    const blend = (front, back) => [0, 1, 2].map(i => front[i] * (front[3] ?? 1) + back[i] * (1 - (front[3] ?? 1)))
    let background = [255, 255, 255]
    const parents = []
    for (let el = element; el; el = el.parentElement) parents.unshift(el)
    for (const el of parents) background = blend(rgba(getComputedStyle(el).backgroundColor), background)
    const foreground = blend(rgba(getComputedStyle(element).color), background)
    const luminance = rgb => rgb.map(x => { const s = x / 255; return s <= .04045 ? s / 12.92 : ((s + .055) / 1.055) ** 2.4 }).reduce((sum, v, i) => sum + v * [.2126, .7152, .0722][i], 0)
    const a = luminance(foreground), b = luminance(background)
    return (Math.max(a, b) + .05) / (Math.min(a, b) + .05)
  })
}

test('filter counts in both states and slash hint meet 4.5:1 against their rendered backgrounds', async () => {
  for (const width of [390, 1200]) {
    const page = await browser.newPage({ viewport: { width, height: 800 } })
    try {
      await catalogue(page, sample)
      for (const active of ['All', 'Incomplete']) {
        await page.getByRole('button', { name: new RegExp(`^${active} `) }).click()
        for (const label of ['All', 'Incomplete']) {
          const ratio = await contrast(page.getByRole('button', { name: new RegExp(`^${label} `) }).locator('small'))
          assert.ok(ratio >= 4.5, `${width}px ${label} count with ${active} active: ${ratio.toFixed(2)}:1`)
        }
        const ratio = await contrast(page.locator('.search kbd'))
        assert.ok(ratio >= 4.5, `${width}px slash hint: ${ratio.toFixed(2)}:1`)
      }
    } finally { await page.close() }
  }
})
