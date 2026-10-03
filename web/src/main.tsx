import React, { useEffect, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { groups, projectNameMatches, size, type Artifact, type Project } from './catalogue'
import { PublishForm, type Dropped } from './PublishForm'
import { droppedFiles } from './publish'
import './style.css'

type ApiFailure = { code: string; message: string; status: number }

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  // All mutations stay on the Portal origin so the browser supplies its
  // same-origin request context for CrossOriginProtection.
  const response = await fetch('/ui/api/' + path, { ...init, credentials: 'same-origin', mode: 'same-origin' })
  if (response.status === 401) { location.reload(); throw new Error('Session expired') }
  if (!response.ok) {
    const body = await response.json().catch(() => null)
    const failure: ApiFailure = {
      code: body?.error?.code || 'request_failed',
      message: body?.error?.message || `Request failed (${response.status})`,
      status: response.status,
    }
    throw failure
  }
  return response.status === 204 ? undefined as T : response.json() as Promise<T>
}

function failureOf(error: unknown): ApiFailure {
  if (typeof error === 'object' && error !== null && 'code' in error && 'message' in error && 'status' in error) return error as ApiFailure
  return { code: 'network_error', message: error instanceof Error ? error.message : String(error), status: 0 }
}

function MutationError({ failure, retry }: { failure: ApiFailure | null; retry: () => void }) {
  if (!failure) return null
  return <p className="mutation-error" role="alert">{failure.code}: {failure.message}{(failure.code === 'busy' || failure.status === 503) && <> <button type="button" onClick={retry}>Retry</button></>}</p>
}

const shortDate = (s: string) => {
  const d = new Date(s)
  return d.toLocaleDateString(undefined, d.getFullYear() === new Date().getFullYear()
    ? { day: 'numeric', month: 'short' }
    : { day: 'numeric', month: 'short', year: 'numeric' })
}

function Icon({ kind }: { kind: 'folder' | 'file' | 'bundle' | 'copy' }) {
  const paths = {
    folder: <path d="M3 7.5A2.5 2.5 0 0 1 5.5 5H9l2 2h7.5A2.5 2.5 0 0 1 21 9.5v7A2.5 2.5 0 0 1 18.5 19h-13A2.5 2.5 0 0 1 3 16.5z" />,
    file: <path d="M7 3h7l5 5v13H7zM14 3v5h5" />,
    bundle: <path d="M4 7l8-4 8 4-8 4zM4 7v10l8 4V7M12 11v10" />,
    copy: <path d="M9 9h10v11H9zM5 15V4h10" />,
  }
  return <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[kind]}</svg>
}

function ProjectHeader({ project, changed, deleted, refresh }: { project: Project; changed: (project: Project) => void; deleted: () => void; refresh: () => Promise<void> }) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(project.description)
  const [pending, setPending] = useState(false)
  const [failure, setFailure] = useState<ApiFailure | null>(null)
  const [confirming, setConfirming] = useState(false)
  const [confirmation, setConfirmation] = useState('')
  async function save() {
    if (pending || confirming) return
    if (draft === project.description) { setEditing(false); setFailure(null); return }
    setPending(true)
    setFailure(null)
    try {
      const updated = await api<Project>('projects/' + encodeURIComponent(project.name), {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ description: draft }),
      })
      changed(updated)
      setEditing(false)
    } catch (error) { setFailure(failureOf(error)) }
    finally { setPending(false) }
  }
  async function remove() {
    if (pending || !projectNameMatches(project.name, confirmation)) return
    setPending(true)
    setFailure(null)
    try {
      await api<void>('projects/' + encodeURIComponent(project.name), { method: 'DELETE' })
      deleted()
    } catch (error) {
      setFailure(failureOf(error))
      try { await refresh() } catch { /* The page-level refresh error remains available. */ }
      setConfirming(false)
      setConfirmation('')
    } finally { setPending(false) }
  }
  async function retryDelete() {
    if (pending) return
    setPending(true)
    try {
      await refresh()
      setConfirmation('')
      setFailure(null)
      setConfirming(true)
    } catch { /* Keep confirmation closed until the current Project state is loaded. */ }
    finally { setPending(false) }
  }
  return <div className="project-head"><Icon kind="folder" /><div className="project-info"><div className="project-name"><h2>{project.name}</h2><code>/{project.name}/</code><span className="project-count">{project.artifact_count} {project.artifact_count === 1 ? 'Artifact' : 'Artifacts'}</span></div>
    <div className="project-description">{editing ? <form onSubmit={e => { e.preventDefault(); void save() }}>
      <label htmlFor={`project-description-${project.name}`}>Project description</label>
      <input id={`project-description-${project.name}`} type="text" value={draft} disabled={pending || confirming} onChange={e => { if (Array.from(e.target.value).length <= 1000) { setDraft(e.target.value); setFailure(null) } }} />
      <button type="submit" disabled={pending || confirming}>Save</button><button type="button" disabled={pending || confirming} onClick={() => { setDraft(project.description); setFailure(null); setEditing(false) }}>Cancel</button>
      <MutationError failure={failure} retry={() => void save()} />
    </form> : <button className="edit-description" type="button" disabled={pending || confirming} onClick={() => { setDraft(project.description); setEditing(true) }} aria-label={`Edit ${project.name} Project description`}>{project.description || 'Add a Project description…'}</button>}</div>
    {confirming ? <div className="project-delete-confirm" role="group" aria-label={`Delete ${project.name} Project`}>
      <p><strong>{project.artifact_count} {project.artifact_count === 1 ? 'Artifact' : 'Artifacts'}</strong> (informational count). The server deletes all Project contents as they exist when accepted, including hidden, nested, Bundle, and Incomplete Artifacts. Public URLs will stop working. This removes the description and cannot be undone.</p>
      <label htmlFor={`confirm-project-${project.name}`}>Type <code>{project.name}</code> to confirm</label>
      <input id={`confirm-project-${project.name}`} value={confirmation} disabled={pending} onChange={e => setConfirmation(e.target.value)} autoComplete="off" />
      <button className="danger" type="button" disabled={pending || !projectNameMatches(project.name, confirmation)} onClick={() => void remove()}>Delete Project</button>
      <button type="button" disabled={pending} onClick={() => { setConfirming(false); setConfirmation(''); setFailure(null) }}>Cancel</button>
    </div> : <>
      <button className="project-delete-button danger" type="button" disabled={pending || editing} onClick={() => { setConfirming(true); setConfirmation(''); setFailure(null); setEditing(false) }}>Delete Project</button>
      {failure && <p className="mutation-error" role="alert">{failure.code}: {failure.message} <button type="button" disabled={pending} onClick={() => void retryDelete()}>Retry</button></p>}
    </>}
  </div></div>
}

function Row({ artifact, base, changed, deleted, publish, form }: { artifact: Artifact; base: string; changed: (artifact: Artifact) => void; deleted: (path: string) => void; publish: () => void; form: React.ReactNode }) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(artifact.description)
  const [confirming, setConfirming] = useState(false)
  const [pending, setPending] = useState(false)
  const [failure, setFailure] = useState<ApiFailure | null>(null)
  const [copyFeedback, setCopyFeedback] = useState('')
  // The URL comes from the configured public host, never the owner-facing host.
  const url = new URL(artifact.path.split('/').map(encodeURIComponent).join('/'), base + '/').href
  const name = artifact.path.replace(/\/$/, '').split('/').at(-1) + (artifact.path.endsWith('/') ? '/' : '')
  async function copyURL() {
    setCopyFeedback('')
    try {
      await navigator.clipboard.writeText(url)
      setCopyFeedback('URL copied to clipboard.')
    } catch {
      setCopyFeedback("Couldn't copy URL. Select the link to copy it manually.")
    }
  }
  async function save() {
    if (pending) return
    if (draft === artifact.description) { setEditing(false); setFailure(null); return }
    setPending(true)
    setFailure(null)
    try {
      const updated = await api<Artifact>('artifacts/' + artifact.path, {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ description: draft }),
      })
      changed(updated)
      setEditing(false)
    } catch (error) { setFailure(failureOf(error)) }
    finally { setPending(false) }
  }
  async function remove() {
    if (pending) return
    setPending(true)
    setFailure(null)
    try {
      await api<void>('artifacts/' + artifact.path, { method: 'DELETE' })
      deleted(artifact.path)
    } catch (error) { setFailure(failureOf(error)) }
    finally { setPending(false) }
  }
  return <div className="row">
    <div className="row-main">
      <span className="row-icon"><Icon kind={artifact.path.endsWith('/') ? 'bundle' : 'file'} /></span>
      <div className="row-content">
        <div className="row-title"><strong>{artifact.title || name}</strong>{artifact.state === 'incomplete' && <span className="badge">Incomplete</span>}</div>
        <div className="artifact-url"><a className="artifact-link" href={url} target="_blank" rel="noopener noreferrer" aria-label={`Open ${artifact.path} on the public host`}>{url}</a>{' '}<button type="button" className="copy-link" aria-label={`Copy link for ${artifact.path}`} title="Copy URL" onClick={() => void copyURL()}><Icon kind="copy" /></button></div>
        <span className="copy-feedback" role="status" aria-live="polite">{copyFeedback}</span>
        {editing ? <form className="description-form" onSubmit={e => { e.preventDefault(); void save() }}>
          <label htmlFor={`artifact-description-${artifact.path}`}>Artifact description</label>
          <textarea id={`artifact-description-${artifact.path}`} value={draft} disabled={pending} onChange={e => { if (Array.from(e.target.value).length <= 1000) { setDraft(e.target.value); setFailure(null) } }} />
          <span className="muted">{Array.from(draft).length}/1,000</span>
          <button type="submit" disabled={pending}>Save</button><button type="button" disabled={pending} onClick={() => { setDraft(artifact.description); setEditing(false); setFailure(null) }}>Cancel</button>
        </form> : <p className="description">{artifact.description || <span className="muted">No private description</span>}</p>}
        {artifact.state === 'incomplete' && <p className="warning">The last publish or delete didn't finish. Readers may see mixed files or 404s. Publish again or delete to finish.</p>}
        <MutationError failure={failure} retry={() => void (confirming ? remove() : save())} />
      </div>
      <div className="row-meta"><time dateTime={artifact.updated_at} title="Last published">{shortDate(artifact.updated_at)}</time><span>{size(artifact.total_size)}</span></div>
      <div className="row-actions">
        {confirming ? <div className="delete-confirm"><p>Readers get 404 at once; there is no undo.</p><button className="danger" type="button" disabled={pending} onClick={() => void remove()}>Confirm delete</button><button type="button" disabled={pending} onClick={() => { setConfirming(false); setFailure(null) }}>Cancel</button></div> : <>
          <button type="button" disabled={pending || editing} onClick={() => { setDraft(artifact.description); setEditing(true); setFailure(null) }} aria-label={`Edit description for ${artifact.path}`}>Edit</button>
          <button type="button" onClick={publish} aria-label={`Publish new version of ${artifact.path}`}>Republish</button>
          <button className="danger" type="button" disabled={pending} onClick={() => { setConfirming(true); setEditing(false); setDraft(artifact.description); setFailure(null) }} aria-label={`Delete ${artifact.path}`}>Delete</button>
        </>}
      </div>
    </div>
    {form}
  </div>
}

function App() {
  const [artifacts, setArtifacts] = useState<Artifact[]>([])
  const [projects, setProjects] = useState<Project[]>([])
  const [base, setBase] = useState('')
  const [email, setEmail] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [refreshError, setRefreshError] = useState('')
  const [query, setQuery] = useState('')
  const [incomplete, setIncomplete] = useState(false)
  const [publishing, setPublishing] = useState<{ slot: string; initial: string; fixed: boolean; selection?: Dropped; id: number } | null>(null)
  const [dragged, setDragged] = useState('')
  const search = useRef<HTMLInputElement>(null)
  async function refreshCatalogue() {
    try {
      const [a, p] = await Promise.all([api<Artifact[]>('artifacts'), api<Project[]>('projects')])
      setArtifacts(a); setProjects(p); setRefreshError('')
    } catch (error) {
      const failure = failureOf(error)
      setRefreshError(`${failure.code}: ${failure.message}`)
      throw error
    }
  }
  function refreshAfterMutation() { void refreshCatalogue().catch(e => setRefreshError(`${failureOf(e).code}: ${failureOf(e).message}`)) }
  function artifactChanged(updated: Artifact) {
    setArtifacts(existing => existing.map(item => item.path === updated.path ? updated : item))
    refreshAfterMutation()
  }
  function published(artifact: Artifact) {
    setPublishing(null)
    setArtifacts(existing => [...existing.filter(a => a.path !== artifact.path), artifact])
    refreshAfterMutation()
  }
  function openPublish(slot: string, initial: string, fixed = false, selection?: Dropped) {
    setPublishing({ slot, initial, fixed, selection, id: Date.now() + Math.random() })
  }
  async function drop(e: React.DragEvent, slot: string, prefix: string) {
    e.preventDefault(); setDragged('')
    try { openPublish(slot, prefix, false, await droppedFiles(e.dataTransfer)) }
    catch (error) { setRefreshError(error instanceof Error ? error.message : String(error)) }
  }
  function form(slot: string) {
    return publishing?.slot === slot && <PublishForm key={publishing.id} initial={publishing.initial} fixed={publishing.fixed} selection={publishing.selection} existing={artifacts} base={base} done={published} close={() => setPublishing(null)} />
  }
  function artifactDeleted(path: string) {
    setArtifacts(existing => existing.filter(item => item.path !== path))
    refreshAfterMutation()
  }
  useEffect(() => {
    Promise.all([api<Artifact[]>('artifacts'), api<Project[]>('projects'), api<{ public_base_url: string }>('config'), api<{ label: string }>('whoami')])
      .then(([a, p, c, me]) => { setArtifacts(a); setProjects(p); setBase(c.public_base_url); setEmail(me.label) })
      .catch(e => setError(`${failureOf(e).code}: ${failureOf(e).message}`)).finally(() => setLoading(false))
  }, [])
  useEffect(() => {
    const shortcut = (e: KeyboardEvent) => {
      if (e.key === '/' && !(e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement || (e.target instanceof HTMLElement && e.target.isContentEditable))) { e.preventDefault(); search.current?.focus() }
    }
    window.addEventListener('keydown', shortcut)
    return () => window.removeEventListener('keydown', shortcut)
  }, [])
  const incompleteCount = artifacts.filter(a => a.state === 'incomplete').length
  const visible = groups(projects, artifacts, query, incomplete)
  return <><header className="top"><div className="top-inner"><span className="mark">p</span><b>pub-hub</b><span className="muted">♙ Private Catalogue</span><span className="spacer" /><span className="muted">{size(artifacts.reduce((sum, a) => sum + a.total_size, 0))} in {artifacts.length} Artifacts</span><span className="divider" /><span className="email">{email}</span><a href="/oauth2/sign_out">Sign out</a></div></header>
    <main><div className="hero"><div><span className="eyebrow">Catalogue</span><h1>Published Artifacts <span className="count">{artifacts.length}</span></h1><p className="muted">Public at {base ? new URL(base).host : 'pub.'}, never indexed or listed.</p></div><div className="hero-actions"><button type="button" className="publish-button" onClick={() => openPublish('top', '')}>Publish</button><span className="muted private">♙ Descriptions are private</span></div></div>
      {refreshError && <p role="alert" className="mutation-error">Could not refresh Catalogue: {refreshError} <button type="button" onClick={() => refreshAfterMutation()}>Retry</button></p>}
      {incompleteCount > 0 && <div className="notice">{incompleteCount} {incompleteCount === 1 ? 'Artifact' : 'Artifacts'} didn't finish publishing or deleting. Publish again or delete to finish.<button onClick={() => setIncomplete(true)}>Show →</button></div>}
      <div className="toolbar"><label className="search"><span>⌕</span><input ref={search} value={query} onChange={e => setQuery(e.target.value)} placeholder="Search Projects, paths, titles, descriptions…" aria-label="Search Catalogue" /><kbd>/</kbd></label><button aria-pressed={!incomplete} onClick={() => setIncomplete(false)}>All <small>{artifacts.length}</small></button><button aria-pressed={incomplete} onClick={() => setIncomplete(true)}>Incomplete <small>{incompleteCount}</small></button><span className="spacer" /><span className="muted">{visible.length} Projects</span></div>
      {form('top')}
      {loading ? <p role="status">Loading Catalogue…</p> : error ? <p role="alert">{error}</p> : visible.length === 0 ? <p className="empty">No matching Artifacts or Projects.</p> : <div className="inventory">{visible.map(g => <section className="project" key={g.project.name}><div className={dragged === g.project.name ? 'drop-target active' : 'drop-target'} onDragOver={e => { e.preventDefault(); setDragged(g.project.name) }} onDragLeave={() => setDragged('')} onDrop={e => void drop(e, g.project.name, g.project.name + '/')}><ProjectHeader project={g.project} changed={updated => { setProjects(existing => existing.map(p => p.name === updated.name ? updated : p)); refreshAfterMutation() }} deleted={() => { setProjects(existing => existing.filter(p => p.name !== g.project.name)); setArtifacts(existing => existing.filter(a => !a.path.startsWith(g.project.name + '/'))); refreshAfterMutation() }} refresh={refreshCatalogue} /></div>{form(g.project.name)}
        {!g.artifacts.length && !g.categories.length && <p className="empty">No Artifacts yet</p>}
        {g.artifacts.map(a => <Row artifact={a} base={base} key={a.path} changed={artifactChanged} deleted={artifactDeleted} publish={() => openPublish(a.path, a.path, true)} form={form(a.path)} />)}
        {g.categories.map(c => <div className="category-group" key={c.name}><h3 className={dragged === g.project.name + '/' + c.name ? 'category drop-target active' : 'category drop-target'} onDragOver={e => { e.preventDefault(); e.stopPropagation(); setDragged(g.project.name + '/' + c.name) }} onDragLeave={e => { e.stopPropagation(); setDragged('') }} onDrop={e => { e.stopPropagation(); void drop(e, g.project.name + '/' + c.name, g.project.name + '/' + c.name + '/') }}>{c.name}/</h3>{form(g.project.name + '/' + c.name)}{c.artifacts.map(a => <Row artifact={a} base={base} key={a.path} changed={artifactChanged} deleted={artifactDeleted} publish={() => openPublish(a.path, a.path, true)} form={form(a.path)} />)}</div>)}
      </section>)}</div>}
      <p className="drop-hint">Drop a file or folder on a Project or Category header to publish into it.</p>
    </main></>
}

createRoot(document.getElementById('root')!).render(<App />)
