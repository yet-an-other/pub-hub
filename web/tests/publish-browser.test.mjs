import { test } from 'node:test'
import { strict as assert } from 'node:assert'
import { startBrowser, stopBrowser, catalogueBody, json } from './catalogue-fixture.mjs'

const artifact = { path: 'fixture/demo.html', url: 'https://pub.example.test/fixture/demo.html', title: 'Demo', description: '', created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z', last_publisher: 'fixture', total_size: 32, file_count: 1, state: 'published' }
const projects = [{ name: 'fixture', description: '', artifact_count: 1 }]
let fixture, browser
test.before(async () => { fixture = await startBrowser(); browser = fixture.browser })
test.after(async () => { await stopBrowser(fixture) })

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
    return route.fulfill(json(catalogueBody(url.pathname, catalogue, projects, 'fixture')))
  })
  await page.goto(fixture.origin)
  await page.getByRole('heading', { name: /Published Artifacts/ }).waitFor()
  return page
}

async function submit(page) {
  await page.getByRole('button', { name: 'Publish', exact: true }).first().click()
  await page.locator('input[type=file]').first().setInputFiles({ name: 'demo.html', mimeType: 'text/html', buffer: Buffer.from('<title>Demo</title>') })
  await page.getByLabel('Artifact path').fill('fixture/demo.html')
  await page.locator('.publish-form button[type=submit]').click()
}

test('transfer progress does not announce publication before the server response', async t => {
  const page = await pageWithFixture(t)
  await page.evaluate(() => {
    window.publishMessages = []
    new MutationObserver(() => {
      const progress = document.querySelector('.publish-form [role=status]')
      const success = document.querySelector('.publish-confirmation')
      if (progress) window.publishMessages.push(progress.textContent)
      if (success) window.publishMessages.push(success.textContent)
    }).observe(document.querySelector('main'), { subtree: true, childList: true, characterData: true })
  })
  await submit(page)
  await page.getByRole('status', { name: /Artifact published/i }).waitFor()
  const messages = await page.evaluate(() => window.publishMessages)
  assert.ok(messages.some(message => message.includes('Publication finishes after the server responds') || message.includes('Waiting for server to finish publishing')), messages.join('\n'))
  assert.ok(messages.indexOf(messages.find(message => message.includes('Artifact published.'))) > 0)
})

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
