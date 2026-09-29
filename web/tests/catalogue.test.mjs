import { test } from 'node:test'
import { strict as assert } from 'node:assert'
import { groups } from '../src/catalogue.ts'
const art = (path, updated_at, state = 'published', description = '') => ({ path, updated_at, state, description, title: path, last_publisher: 'agent' })
const projects = [{ name: 'empty', description: 'keep me', artifact_count: 0 }, { name: 'older', description: '', artifact_count: 2 }, { name: 'newer', description: '', artifact_count: 1 }]
const items = [art('older/z.html', '2025-01-01'), art('older/a/b/', '2025-01-02', 'incomplete'), art('newer/demo.html', '2025-02-01')]
test('recent Projects first, empty described Projects last, direct Artifacts before Categories', () => {
  const result = groups(projects, items, '', false)
  assert.deepEqual(result.map(g => g.project.name), ['newer', 'older', 'empty'])
  assert.deepEqual(result[1].artifacts.map(a => a.path), ['older/z.html'])
  assert.deepEqual(result[1].categories.map(c => c.name), ['a'])
})
test('Incomplete filter and search across Project, path, description and Publisher', () => {
  assert.deepEqual(groups(projects, items, '', true).map(g => g.project.name), ['older'])
  for (const q of ['older', 'a/b/', 'agent']) assert.ok(groups(projects, items, q, false).some(g => g.project.name === 'older'))
  assert.deepEqual(groups(projects, items, 'keep me', false).map(g => g.project.name), ['empty'])
})
