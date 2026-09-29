export type PublishProblem = { code: string; message: string }
export type ChosenFile = { path: string; file: File }

// Mirrors internal/naming/path.go. The Portal remains authoritative.
export function validatePath(path: string): PublishProblem | null {
  if (path.length > 200) return { code: 'name_invalid', message: 'Path exceeds 200 characters' }
  const stem = path.endsWith('.html') ? path.slice(0, -5) : path.endsWith('/') ? path.slice(0, -1) : ''
  if (!stem) return { code: 'name_invalid', message: 'Use .html for a file or / for a Bundle' }
  const parts = stem.split('/')
  if (parts.length < 2) return { code: 'name_invalid', message: 'An Artifact must be below a Project' }
  for (const part of parts) {
    if (!part || part.length > 63 || !/^[a-z0-9]+(-[a-z0-9]+)*$/.test(part)) return { code: 'name_invalid', message: `Invalid path segment: ${part || '(empty)'}` }
    if (part === 'index' || part === 'cdn-cgi') return { code: 'name_reserved', message: `Reserved path segment: ${part}` }
  }
  return null
}

const stem = (path: string) => path.endsWith('/') ? path.slice(0, -1) : path.slice(0, -5)
export function conflict(path: string, existing: string[]): 'replace' | 'shape_conflict' | 'nesting_conflict' | null {
  const candidate = stem(path)
  for (const other of existing) {
    const current = stem(other)
    if (candidate === current) return path === other ? 'replace' : 'shape_conflict'
    if (candidate.startsWith(current + '/') || current.startsWith(candidate + '/')) return 'nesting_conflict'
  }
  return null
}

export async function prepareFiles(files: ChosenFile[], bundle: boolean): Promise<{ files: ChosenFile[]; skipped: string[]; title: string; error: PublishProblem | null }> {
  const skipped = files.filter(f => f.path.split('/').some(p => p.startsWith('.'))).map(f => f.path)
  const kept = files.filter(f => !skipped.includes(f.path))
  let error: PublishProblem | null = null
  if (!kept.length) error = { code: 'file_count', message: 'Choose a file to publish' }
  else if (bundle && !kept.some(f => f.path === 'index.html')) error = { code: 'index_missing', message: 'Bundle needs index.html at its root' }
  else if (!bundle && (kept.length !== 1 || !kept[0].path.endsWith('.html'))) error = { code: 'file_count', message: 'Choose one HTML file' }
  else if (kept.length > 2000) error = { code: 'too_many_files', message: 'Bundle exceeds 2,000 files' }
  const entry = bundle ? kept.find(f => f.path === 'index.html') : kept[0]
  const text = entry ? await entry.file.slice(0, 65536).text() : ''
  const title = typeof DOMParser === 'undefined'
    ? (text.match(/<title(?:\s[^>]*)?>([\s\S]*?)<\/title\s*>/i)?.[1] || '').trim()
    : new DOMParser().parseFromString(text, 'text/html').querySelector('title')?.textContent?.trim() || ''
  return { files: kept, skipped, title, error }
}

interface Entry { name: string; isFile: boolean; isDirectory: boolean; file: (success: (file: File) => void, failure: (error: DOMException) => void) => void; createReader: () => { readEntries: (success: (entries: Entry[]) => void, failure: (error: DOMException) => void) => void } }
async function walk(entry: Entry, prefix = ''): Promise<ChosenFile[]> {
  const path = prefix + entry.name
  if (entry.isFile) return [{ path, file: await new Promise<File>((resolve, reject) => entry.file(resolve, reject)) }]
  if (!entry.isDirectory) return []
  const reader = entry.createReader()
  const files: ChosenFile[] = []
  for (;;) {
    // Chromium may return directory entries in batches.
    const batch = await new Promise<Entry[]>((resolve, reject) => reader.readEntries(resolve, reject))
    if (!batch.length) break
    for (const child of batch) files.push(...await walk(child, path + '/'))
  }
  return files
}

export async function droppedFiles(transfer: DataTransfer): Promise<{ files: ChosenFile[]; bundle: boolean; name: string }> {
  const items = Array.from(transfer.items).filter(item => item.kind === 'file')
  const entries = items.map(item => item.webkitGetAsEntry?.() as unknown as Entry | null).filter((entry): entry is Entry => !!entry)
  if (entries.length) {
    if (entries.length !== 1) throw new Error('Drop one file or one folder')
    const entry = entries[0]
    const files = await walk(entry)
    return { files: entry.isDirectory ? files.map(f => ({ ...f, path: f.path.slice(entry.name.length + 1) })) : files, bundle: entry.isDirectory, name: entry.name }
  }
  const files = Array.from(transfer.files)
  if (files.length !== 1) throw new Error('Drop one file or one folder')
  return { files: [{ path: files[0].name, file: files[0] }], bundle: false, name: files[0].name }
}
