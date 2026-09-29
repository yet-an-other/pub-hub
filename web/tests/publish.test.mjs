import { test } from 'node:test'
import { strict as assert } from 'node:assert'
import { validatePath, conflict, prepareFiles, droppedFiles } from '../src/publish.ts'

const paths = ['xform/notes/plan.html', 'xform/site/', 'xform/notes/deep/page.html']
test('Portal naming cases', () => {
  for (const path of ['xform/plan.html', 'xform/notes/roster-sync/']) assert.equal(validatePath(path), null)
  for (const path of ['xform/plan', 'xform.html', 'xform/Plan.html', 'xform/plan.v2.html', '/xform/plan.html', 'xform//plan.html', 'xform/my--plan.html', `xform/${'a'.repeat(64)}.html`, `project/${'a'.repeat(63)}/${'b'.repeat(63)}/${'c'.repeat(63)}.html`]) assert.equal(validatePath(path)?.code, 'name_invalid', path)
  for (const path of ['xform/notes/index.html', 'xform/cdn-cgi/plan.html', 'index/plan.html']) assert.equal(validatePath(path)?.code, 'name_reserved', path)
})
test('shape, nesting and replacement against loaded Catalogue', () => {
  assert.equal(conflict('xform/notes/plan.html', paths), 'replace')
  assert.equal(conflict('xform/notes/plan/', paths), 'shape_conflict')
  assert.equal(conflict('xform/site/file.html', paths), 'nesting_conflict')
  assert.equal(conflict('xform/notes/', paths), 'nesting_conflict')
  assert.equal(conflict('xform/notes/deep/', paths), 'nesting_conflict')
  assert.equal(conflict('xform/other.html', paths), null)
})
test('directory drops keep relative paths across entry batches', async () => {
  const leaf = name => ({ name, isFile: true, file: resolve => resolve({ name, size: 1 }) })
  const directory = (name, batches) => ({ name, isDirectory: true, createReader: () => ({ readEntries: resolve => resolve(batches.shift() || []) }) })
  const root = directory('demo', [[leaf('index.html'), directory('assets', [[leaf('app.js')], []])], []])
  const selection = await droppedFiles({ items: [{ kind: 'file', webkitGetAsEntry: () => root }] })
  assert.equal(selection.name, 'demo')
  assert.equal(selection.bundle, true)
  assert.deepEqual(selection.files.map(f => f.path), ['index.html', 'assets/app.js'])
})
test('dot-files skipped at every level, and Bundle entry required', async () => {
  const file = (name, text) => ({ name, size: text.length, slice: () => ({ text: async () => text }) })
  const result = await prepareFiles([
    { path: 'index.html', file: file('index.html', '<title>Hello</title>') },
    { path: '.hidden', file: file('.hidden', '') },
    { path: 'assets/.cache/data', file: file('data', '') },
    { path: 'assets/app.js', file: file('app.js', 'js') },
  ], true)
  assert.deepEqual(result.files.map(f => f.path), ['index.html', 'assets/app.js'])
  assert.deepEqual(result.skipped, ['.hidden', 'assets/.cache/data'])
  assert.equal(result.title, 'Hello')
  assert.equal(result.error, null)
  assert.equal((await prepareFiles([{ path: 'a.html', file: file('a.html', '') }], true)).error?.code, 'index_missing')
})
