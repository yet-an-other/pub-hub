import { test, before, after } from 'node:test'
import { strict as assert } from 'node:assert'
import { startBrowser, stopBrowser, catalogueBody, json } from './catalogue-fixture.mjs'

const publicURL = 'https://pub.example.test/demo/notes.html'
const artifacts = [{ path: 'demo/notes.html', title: 'Notes', description: '', updated_at: '2025-01-01', total_size: 12, state: 'published', last_publisher: 'agent' }]
const projects = [{ name: 'demo', description: '', artifact_count: 1 }]
let fixture, browser
before(async () => { fixture = await startBrowser(); browser = fixture.browser })
after(async () => { await stopBrowser(fixture) })

async function catalogue(width, failCopy = false) {
  const context = await browser.newContext({ viewport: { width, height: 800 }, permissions: ['clipboard-read', 'clipboard-write'] })
  const page = await context.newPage()
  if (failCopy) await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('Permission denied') } } })
  })
  await page.route('**/ui/api/**', route => {
    const path = new URL(route.request().url()).pathname
    return route.fulfill(json(catalogueBody(path, artifacts, projects)))
  })
  await page.goto(fixture.origin)
  const link = page.getByRole('link', { name: 'Open demo/notes.html on the public host' })
  await link.waitFor()
  return { context, page, link, copy: page.getByRole('button', { name: 'Copy link for demo/notes.html' }) }
}

for (const width of [1100, 390]) {
  test(`Catalogue copies the public URL with announced feedback at ${width}px`, async () => {
    const { context, page, link, copy } = await catalogue(width)
    try {
      assert.equal(await link.getAttribute('href'), publicURL)
      assert.equal(await link.innerText(), publicURL)
      const box = await copy.boundingBox()
      assert.ok(box.width >= 40 && box.height >= 40, `copy target is ${box.width}x${box.height}`)
      const linkBox = await link.boundingBox()
      assert.ok(box.x >= linkBox.x + linkBox.width || box.y >= linkBox.y + linkBox.height, 'copy target does not cover the link')
      await context.route('https://pub.example.test/**', route => route.fulfill({ body: '<title>Public Artifact</title>', contentType: 'text/html' }))
      const popupPromise = page.waitForEvent('popup')
      await link.click()
      const popup = await popupPromise
      await popup.waitForURL(publicURL)
      assert.equal(popup.url(), publicURL)
      await popup.close()
      await page.keyboard.press('Tab')
      assert.equal(await copy.evaluate(el => el === document.activeElement), true)
      assert.notEqual(await copy.evaluate(el => getComputedStyle(el).outlineStyle), 'none')
      await copy.click()
      assert.equal(await page.evaluate(() => navigator.clipboard.readText()), publicURL)
      assert.match(await page.getByRole('status').innerText(), /copied/i)
      assert.equal(await link.isVisible(), true)
    } finally { await context.close() }
  })

  test(`Catalogue explains clipboard failure and keeps the URL selectable at ${width}px`, async () => {
    const { context, page, link, copy } = await catalogue(width, true)
    try {
      await copy.click()
      assert.match(await page.getByRole('status').innerText(), /couldn't copy|copy failed/i)
      assert.equal(await link.innerText(), publicURL)
      assert.equal(await link.evaluate(el => getComputedStyle(el).userSelect), 'auto')
      assert.equal(await link.isVisible(), true)
    } finally { await context.close() }
  })
}
