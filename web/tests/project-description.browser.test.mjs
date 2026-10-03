import { test, before, after } from 'node:test'
import { strict as assert } from 'node:assert'
import { startBrowser, stopBrowser, catalogueBody, json } from './catalogue-fixture.mjs'

let fixture, browser
before(async () => { fixture = await startBrowser(); browser = fixture.browser })
after(async () => { await stopBrowser(fixture) })

const longDescription = `First line of private Project notes.\nSecond line: ${'longword'.repeat(35)} ends here.`

async function catalogue(width, description) {
  const page = await browser.newPage({ viewport: { width, height: 800 } })
  let current = description
  const patches = []
  await page.route('**/ui/api/**', async route => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (request.method() === 'PATCH' && path === '/ui/api/projects/xform') {
      current = request.postDataJSON().description
      patches.push(current)
    }
    const projects = [{ name: 'xform', description: current, artifact_count: description ? 0 : 1 }]
    const artifacts = description ? [] : [{ path: 'xform/example.html', title: 'Example', description: '', state: 'published', updated_at: '2025-01-01', total_size: 42, last_publisher: 'owner' }]
    await route.fulfill(json(path === '/ui/api/projects/xform' ? projects[0] : catalogueBody(path, artifacts, projects)))
  })
  await page.goto(fixture.origin)
  await page.getByRole('heading', { name: 'xform' }).waitFor()
  return { page, patches }
}

for (const width of [375, 1200]) {
  test(`populated Project description remains readable and editable at ${width}px`, async () => {
    const { page, patches } = await catalogue(width, longDescription)
    try {
      const section = page.locator('section.project')
      const description = section.getByText(longDescription, { exact: true })
      assert.equal(await description.count(), 1)
      const layout = await description.evaluate(el => {
        const style = getComputedStyle(el)
        return { whiteSpace: style.whiteSpace, overflow: style.overflow, scrollWidth: el.scrollWidth, clientWidth: el.clientWidth, scrollHeight: el.scrollHeight, clientHeight: el.clientHeight, right: el.getBoundingClientRect().right }
      })
      assert.equal(layout.whiteSpace, 'pre-wrap')
      assert.ok(layout.overflow !== 'hidden' && layout.scrollWidth <= layout.clientWidth && layout.scrollHeight <= layout.clientHeight && layout.right <= width, JSON.stringify(layout))
      const edit = section.getByRole('button', { name: 'Edit description for xform', exact: true })
      assert.equal(await edit.isVisible(), true)
      await edit.focus()
      await page.keyboard.press('Enter')
      const input = section.getByRole('textbox', { name: 'Project description' })
      await input.fill('Changed private notes')
      await section.getByRole('button', { name: 'Cancel' }).click()
      assert.equal(await description.isVisible(), true)
      assert.deepEqual(patches, [])
      await edit.click()
      await input.fill('Changed private notes')
      await section.getByRole('button', { name: 'Save' }).click()
      await section.locator('.project-description-text').getByText('Changed private notes', { exact: true }).waitFor()
      assert.deepEqual(patches, ['Changed private notes'])
    } finally { await page.close() }
  })

  test(`empty Project description has a visible edit control at ${width}px`, async () => {
    const { page, patches } = await catalogue(width, '')
    try {
      const section = page.locator('section.project')
      assert.equal(await section.locator('.project-description-text').getByText('No private description', { exact: true }).isVisible(), true)
      const edit = section.getByRole('button', { name: 'Edit description for xform', exact: true })
      await edit.focus()
      await page.keyboard.press('Space')
      await section.getByRole('textbox', { name: 'Project description' }).fill('Added notes')
      await section.getByRole('button', { name: 'Save' }).click()
      await section.locator('.project-description-text').getByText('Added notes', { exact: true }).waitFor()
      assert.deepEqual(patches, ['Added notes'])
    } finally { await page.close() }
  })
}
