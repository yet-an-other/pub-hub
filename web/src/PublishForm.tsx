import { useEffect, useRef, useState } from 'react'
import { conflict, droppedFiles, prepareFiles, validatePath, type ChosenFile } from './publish'
import { size } from './catalogue'
import type { Artifact } from './catalogue'

type Selection = { files: ChosenFile[]; bundle: boolean; name: string }
type Failure = { code: string; message: string; status: number }

export function PublishForm({ initial, fixed, selection, existing, base, done, close }: { initial: string; fixed: boolean; selection?: Selection; existing: Artifact[]; base: string; done: (artifact: Artifact) => void; close: () => void }) {
  const [chosen, setChosen] = useState<Selection | undefined>(selection)
  const [prepared, setPrepared] = useState<Awaited<ReturnType<typeof prepareFiles>> | null>(null)
  const [path, setPath] = useState(initial + (selection && !fixed ? selection.name + (selection.bundle ? '/' : '') : ''))
  const [description, setDescription] = useState('')
  const [noOverwrite, setNoOverwrite] = useState(false)
  const [progress, setProgress] = useState<number | null>(null)
  const [failure, setFailure] = useState<Failure | null>(null)
  const directoryInput = useRef<HTMLInputElement>(null)
  const selectionVersion = useRef(0)
  const pending = progress !== null
  function choose(next: Selection) {
    selectionVersion.current++
    setChosen(next); setFailure(null)
    setPrepared(null)
    if (!fixed) setPath(initial + (next.bundle ? next.name + '/' : next.name))
  }
  useEffect(() => {
    let current = true
    const version = selectionVersion.current
    if (chosen) void prepareFiles(chosen.files, chosen.bundle).then(result => { if (current && version === selectionVersion.current) setPrepared(result) })
    return () => { current = false }
  }, [chosen])
  const validation = validatePath(path)
  const clash = validation ? null : conflict(path, existing.map(a => a.path))
  const shape = chosen && path ? chosen.bundle === path.endsWith('/') : true
  const problem = validation || (shape ? null : { code: 'name_invalid', message: chosen?.bundle ? 'A folder needs a trailing /' : 'A file needs .html' }) || (clash === 'shape_conflict' || clash === 'nesting_conflict' ? { code: clash, message: clash === 'shape_conflict' ? 'Delete the other Artifact shape first' : 'An Artifact cannot contain or be contained by another Artifact' } : null) || prepared?.error
  function upload() {
    if (!chosen || !prepared || problem || pending) return
    setFailure(null); setProgress(0)
    const body = new FormData()
    if (description !== '') body.append('description', description)
    for (const { path: key, file } of prepared.files) body.append(chosen.bundle ? key : file.name, file, file.name)
    const xhr = new XMLHttpRequest()
    xhr.open('PUT', '/ui/api/artifacts/' + path.split('/').map(encodeURIComponent).join('/'))
    xhr.withCredentials = true
    if (noOverwrite) xhr.setRequestHeader('If-None-Match', '*')
    xhr.upload.onprogress = event => { if (event.lengthComputable) setProgress(Math.min(100, Math.round(event.loaded / event.total * 100))) }
    xhr.upload.onload = () => setProgress(100)
    xhr.onload = () => {
      setProgress(null)
      if (xhr.status === 401) { location.reload(); return }
      let result: unknown
      try { result = JSON.parse(xhr.responseText) } catch { /* nginx errors may not be JSON */ }
      if (xhr.status >= 200 && xhr.status < 300) { done(result as Artifact); return }
      const error = (result as { error?: { code?: string; message?: string } } | undefined)?.error
      setFailure({ code: error?.code || 'request_failed', message: error?.message || `Request failed (${xhr.status})`, status: xhr.status })
    }
    xhr.onerror = () => { setProgress(null); setFailure({ code: 'network_error', message: 'Upload failed', status: 0 }) }
    xhr.send(body)
  }
  return <form className="publish-form" onSubmit={e => { e.preventDefault(); upload() }}>
    <h3>Publish Artifact</h3>
    <div className="choose"><label>Choose HTML file <input type="file" accept=".html" disabled={pending} onChange={e => { const file = e.target.files?.[0]; if (file) choose({ files: [{ path: file.name, file }], bundle: false, name: file.name }) }} /></label>
      <label>Choose folder <input ref={node => { directoryInput.current = node; node?.setAttribute('webkitdirectory', '') }} type="file" multiple disabled={pending} onChange={e => { const files = Array.from(e.target.files || []); if (files.length) { const name = files[0].webkitRelativePath.split('/')[0]; choose({ files: files.map(file => ({ path: file.webkitRelativePath.slice(name.length + 1), file })), bundle: true, name }) } }} /></label></div>
    {chosen && <p className="muted">{chosen.bundle ? 'Bundle' : 'HTML file'} · {prepared?.files.length ?? '…'} files · {size(prepared?.files.reduce((n, f) => n + f.file.size, 0) || 0)} · Title: {prepared?.title || '(none)'}</p>}
    {!!prepared?.skipped.length && <p className="warning">Skipped dot-files: {prepared.skipped.join(', ')}</p>}
    <label className="path-label">Path <span>{base ? new URL(base).host : 'pub.bdgn.me'}/</span><input aria-label="Artifact path" value={path} readOnly={fixed} disabled={pending} onChange={e => { setPath(e.target.value); setFailure(null) }} /></label>
    {problem && <p role="alert" className="mutation-error">{problem.code}: {problem.message}</p>}
    {clash === 'replace' && <p className="warning">This will replace the existing Artifact at this URL.</p>}
    <label className="path-label">Description <span className="muted">{clash === 'replace' ? 'Leave empty to keep the current description' : 'Optional, private'}</span><textarea value={description} maxLength={1000} disabled={pending} onChange={e => setDescription(e.target.value)} /></label>
    <label><input type="checkbox" checked={noOverwrite} disabled={pending} onChange={e => setNoOverwrite(e.target.checked)} /> Don't overwrite</label>
    {progress !== null && <div role="status">{progress === 100 ? 'Files transferred. Waiting for server to finish publishing…' : 'Transferring files… Publication finishes after the server responds.'} <progress max="100" value={progress} /> {progress}%</div>}
    {failure && <p className="mutation-error" role="alert">{failure.code}: {failure.message}{(failure.code === 'busy' || failure.status === 503) && <> <button type="button" onClick={upload}>Retry</button></>}</p>}
    <div className="publish-actions"><button type="submit" disabled={!chosen || !prepared || !!problem || pending}>Publish</button><button type="button" disabled={pending} onClick={close}>Cancel</button></div>
  </form>
}
export type Dropped = Awaited<ReturnType<typeof droppedFiles>>
