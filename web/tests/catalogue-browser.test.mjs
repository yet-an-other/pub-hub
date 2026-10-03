import { test, before, after } from 'node:test'
import { strict as assert } from 'node:assert'
import { startBrowser, stopBrowser, catalogueBody, json } from './catalogue-fixture.mjs'

let fixture, browser
before(async () => { fixture = await startBrowser(); browser = fixture.browser })
after(async () => { await stopBrowser(fixture) })
async function catalogue(page, artifacts, projects = [{ name: 'alpha', description: 'Published examples', artifact_count: artifacts.length }]) {
  await page.route('**/ui/api/**', route => {
    const path = new URL(route.request().url()).pathname
    return route.fulfill(json(catalogueBody(path, artifacts, projects)))
  })
  await page.goto(fixture.origin)
  await page.getByRole('button', { name: /^Incomplete \d+$/ }).waitFor()
}
const sample = [{ path: 'alpha/guide.html', title: 'Guide', description: '', state: 'published', total_size: 12, updated_at: '2025-01-01T00:00:00Z', last_publisher: 'owner' }]

// Browser-visible Catalogue seam: safe, read-only API fixtures; no live Portal or Artifact host.
test('Project actions align right and Artifact rows show the last publisher', async () => {
  for (const width of [390, 1200]) {
    const page = await browser.newPage({ viewport: { width, height: 800 } })
    try {
      await catalogue(page, [{ ...sample[0], last_publisher: 'Ada Lovelace' }])
      const project = page.locator('.project')
      const edit = project.getByRole('button', { name: 'Edit description for alpha', exact: true })
      const remove = project.getByRole('button', { name: 'Delete Project' })
      const [editBox, removeBox] = await Promise.all([edit.boundingBox(), remove.boundingBox()])
      assert.equal(editBox.x + editBox.width, removeBox.x + removeBox.width)
      assert.ok(editBox.y + editBox.height <= removeBox.y)
      assert.equal(await project.locator('.row-meta').getByText('Ada Lovelace').isVisible(), true)
      assert.equal(await page.locator('.top .email').innerText(), 'owner@example.test')
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    } finally { await page.close() }
  }
})
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

test('zero Incomplete Artifacts takes precedence over a search and offers Show all', async () => {
  for (const width of [390, 1200]) {
    const page = await browser.newPage({ viewport: { width, height: 800 } })
    try {
      await catalogue(page, sample)
      await page.getByRole('textbox', { name: 'Search Catalogue' }).fill('absent')
      await page.getByRole('button', { name: 'Incomplete 0' }).click()
      assert.match(await page.locator('.catalogue-empty').innerText(), /No Incomplete Artifacts/)
      assert.doesNotMatch(await page.locator('.catalogue-empty').innerText(), /No results for/)
      await page.getByRole('button', { name: 'Show all' }).focus()
      await page.keyboard.press('Enter')
      assert.equal(await page.getByRole('textbox', { name: 'Search Catalogue' }).inputValue(), '')
      assert.equal(await page.getByText('Guide', { exact: true }).count(), 1)
      assert.equal(await page.getByRole('button', { name: 'All 1' }).getAttribute('aria-pressed'), 'true')
    } finally { await page.close() }
  }
})

test('search with no matches offers keyboard-clear search and filters independently', async () => {
  for (const width of [390, 1200]) {
    const page = await browser.newPage({ viewport: { width, height: 800 } })
    try {
      await catalogue(page, [...sample, { ...sample[0], path: 'alpha/unfinished.html', title: 'Unfinished', state: 'incomplete' }])
      await page.keyboard.press('/')
      assert.equal(await page.evaluate(() => document.activeElement?.getAttribute('aria-label')), 'Search Catalogue')
      await page.keyboard.type('absent')
      assert.match(await page.locator('main').innerText(), /No results for/)
      await page.getByRole('button', { name: 'Clear search' }).focus()
      await page.keyboard.press('Enter')
      assert.equal(await page.getByText('Guide', { exact: true }).count(), 1)
      await page.getByRole('button', { name: 'Incomplete 1' }).click()
      await page.getByRole('textbox', { name: 'Search Catalogue' }).fill('absent')
      await page.getByRole('button', { name: 'Clear search and show all' }).focus()
      await page.keyboard.press('Enter')
      assert.equal(await page.getByText('Guide', { exact: true }).count(), 1)
      assert.equal(await page.getByRole('textbox', { name: 'Search Catalogue' }).inputValue(), '')
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    } finally { await page.close() }
  }
})

test('63-character Project name wraps without hiding its path or count', async () => {
  const name = 'a'.repeat(63)
  for (const width of [375, 1200]) {
    const page = await browser.newPage({ viewport: { width, height: 800 } })
    try {
      await catalogue(page, [{ ...sample[0], path: `${name}/guide.html` }], [{ name, description: '', artifact_count: 1 }])
      const header = page.locator('.project-name')
      const metrics = await header.evaluate(el => {
        const parts = [...el.children].map(child => {
          const rect = child.getBoundingClientRect()
          return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height, scrollWidth: child.scrollWidth, clientWidth: child.clientWidth }
        })
        return { parts, scrollWidth: document.documentElement.scrollWidth, viewport: innerWidth }
      })
      assert.ok(metrics.scrollWidth <= width, JSON.stringify(metrics))
      for (const part of metrics.parts) {
        assert.ok(part.left >= 0 && part.right <= width && part.scrollWidth <= part.clientWidth, JSON.stringify(metrics))
      }
      for (let i = 0; i < metrics.parts.length; i++) for (let j = i + 1; j < metrics.parts.length; j++) {
        const a = metrics.parts[i], b = metrics.parts[j]
        assert.ok(a.right <= b.left || b.right <= a.left || a.bottom <= b.top || b.bottom <= a.top, JSON.stringify(metrics))
      }
      assert.equal(await header.locator('code').innerText(), `/${name}/`)
      assert.equal(await header.locator('.project-count').innerText(), '1 Artifact')
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
