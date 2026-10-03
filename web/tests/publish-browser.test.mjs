import { test } from 'node:test'
import { strict as assert } from 'node:assert'
import { spawn } from 'node:child_process'
import { createServer } from 'node:net'
import { existsSync } from 'node:fs'
import { chromium } from 'playwright-core'

const artifact = { path: 'fixture/demo.html', url: 'https://pub.example.test/fixture/demo.html', title: 'Demo', description: '', created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z', last_publisher: 'fixture', total_size: 32, file_count: 1, state: 'published' }
const projects = [{ name: 'fixture', description: '', artifact_count: 1 }]
let server, browser, port
const json = body => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })

test.before(async () => {
  const socket = createServer()
  await new Promise(resolve => socket.listen(0, '127.0.0.1', resolve))
  port = socket.address().port
  await new Promise(resolve => socket.close(resolve))
  server = spawn('node', ['node_modules/vite/bin/vite.js', '--host', '127.0.0.1', '--port', String(port), '--strictPort'], { cwd: new URL('../', import.meta.url), stdio: 'ignore' })
  for (let i = 0; i < 100; i++) {
    try { const response = await fetch(`http://127.0.0.1:${port}/`); if (response.ok) break } catch { /* startup */ }
    if (i === 99) throw new Error('Vite did not start')
    await new Promise(resolve => setTimeout(resolve, 100))
  }
  browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || (existsSync('/usr/bin/chromium') ? '/usr/bin/chromium' : chromium.executablePath()), headless: true, args: ['--no-sandbox'] })
})
test.after(async () => { await browser?.close(); server?.kill() })

async function pageWithFixture(t, { items = [], publish = json(artifact) } = {}) {
  const page = await browser.newPage()
  t.after(() => page.close())
  let catalogue = items
  await page.route('**/ui/api/**', route => {
    const url = new URL(route.request().url())
    if (route.request().method() === 'PUT') {
      return Promise.resolve(publish).then(response => {
        if (response.status >= 200 && response.status < 300) catalogue = [...items.filter(item => item.path !== artifact.path), artifact]
        return route.fulfill(response)
      })
    }
    return route.fulfill(json(url.pathname.endsWith('/artifacts') ? catalogue : url.pathname.endsWith('/projects') ? projects : url.pathname.endsWith('/config') ? { public_base_url: 'https://pub.example.test' } : { label: 'fixture' }))
  })
  await page.goto(`http://127.0.0.1:${port}/`)
  await page.getByRole('heading', { name: /Published Artifacts/ }).waitFor()
  return page
}

async function submit(page) {
  await page.getByRole('button', { name: 'Publish', exact: true }).first().click()
  await page.locator('input[type=file]').first().setInputFiles({ name: 'demo.html', mimeType: 'text/html', buffer: Buffer.from('<title>Demo</title>') })
  await page.getByLabel('Artifact path').fill('fixture/demo.html')
  await page.locator('.publish-form button[type=submit]').click()
}

test('server failure keeps the form open with a retry and no success confirmation', async t => {
  const page = await pageWithFixture(t, { publish: { status: 503, contentType: 'application/json', body: JSON.stringify({ error: { code: 'storage_unavailable', message: 'Storage is unavailable' } }) } })
  await submit(page)
  await page.getByRole('alert').filter({ hasText: 'storage_unavailable: Storage is unavailable' }).waitFor()
  assert.equal(await page.getByRole('button', { name: 'Retry' }).count(), 1)
  assert.equal(await page.getByRole('status', { name: /Artifact published/i }).count(), 0)
  assert.equal(await page.getByLabel('Artifact path').inputValue(), 'fixture/demo.html')
})

test('a later failed publish does not leave an earlier success announcement', async t => {
  const page = await pageWithFixture(t)
  await submit(page)
  await page.getByRole('status', { name: /Artifact published/i }).waitFor()
  await page.route('**/ui/api/artifacts/fixture/demo.html', route => route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: { code: 'storage_unavailable', message: 'Storage is unavailable' } }) }))
  await submit(page)
  await page.getByRole('alert').filter({ hasText: 'storage_unavailable' }).waitFor()
  assert.equal(await page.getByRole('status', { name: /Artifact published/i }).count(), 0)
})

test('filtered success remains visible and Show in Catalogue clears search and Incomplete', async t => {
  const page = await pageWithFixture(t, { items: [{ ...artifact, path: 'fixture/old.html', url: 'https://pub.example.test/fixture/old.html', state: 'incomplete' }] })
  await page.getByLabel('Search Catalogue').fill('old')
  await page.getByRole('button', { name: /Incomplete/ }).click()
  await submit(page)
  const confirmation = page.getByRole('status', { name: /published/i })
  await confirmation.waitFor()
  assert.equal(await page.getByRole('link', { name: `Open ${artifact.path} on the public host` }).count(), 0)
  await confirmation.getByRole('button', { name: 'Show in Catalogue' }).click()
  assert.equal(await page.getByLabel('Search Catalogue').inputValue(), '')
  assert.equal(await page.getByRole('button', { name: /All/ }).getAttribute('aria-pressed'), 'true')
  await page.getByRole('link', { name: `Open ${artifact.path} on the public host` }).waitFor()
  assert.equal(await confirmation.isVisible(), true)
})

test('unfiltered success confirms completed publishing with public URL and Catalogue action', async t => {
  const page = await pageWithFixture(t)
  await submit(page)
  const confirmation = page.getByRole('status', { name: /published/i })
  await confirmation.waitFor()
  assert.equal(await confirmation.getByRole('link', { name: artifact.url }).getAttribute('href'), artifact.url)
  await confirmation.getByRole('button', { name: 'Show in Catalogue' }).click()
  await page.getByRole('link', { name: `Open ${artifact.path} on the public host` }).waitFor()
  assert.equal(await confirmation.isVisible(), true)
})
