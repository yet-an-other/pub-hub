import React, { useEffect, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { groups, type Artifact, type Project } from './catalogue'
import './style.css'

async function api<T>(path: string): Promise<T> {
  const response = await fetch('/ui/api/' + path, { credentials: 'same-origin' })
  if (response.status === 401) { location.reload(); throw new Error('Session expired') }
  if (!response.ok) throw new Error(`Could not load ${path} (${response.status})`)
  return response.json() as Promise<T>
}

const size = (n: number) => n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1048576).toFixed(1)} MB`
const date = (s: string) => new Date(s).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })

function Row({ artifact, base }: { artifact: Artifact; base: string }) {
  const [open, setOpen] = useState(false)
  // The URL comes from the configured public host, never the owner-facing host.
  const url = new URL(artifact.path.split('/').map(encodeURIComponent).join('/'), base + '/').href
  return <div className="row">
    <button className="row-button" aria-expanded={open} onClick={() => setOpen(!open)}>
      <span className="kind" aria-hidden="true">{artifact.path.endsWith('/') ? '▣' : '▤'}</span>
      <span className="identity"><strong>{artifact.title || artifact.path.replace(/\/$/, '').split('/').at(-1)}</strong><small>{artifact.path.replace(/\/$/, '').split('/').at(-1)}{artifact.path.endsWith('/') ? '/' : ''}</small></span>
      {artifact.state === 'incomplete' && <span className="badge">Incomplete</span>}
      <span className="summary">{artifact.description}</span><span className="updated">{date(artifact.updated_at)}</span><span className="bytes">{size(artifact.total_size)}</span><span aria-hidden="true">{open ? '⌃' : '⌄'}</span>
    </button>
    {open && <div className="details">
      <a className="preview" href={url} target="_blank" rel="noopener noreferrer" aria-label={`Open ${artifact.title} on the public host`}>
        <iframe title={`Preview of ${artifact.title}`} src={url} tabIndex={-1} loading="lazy" sandbox="allow-scripts allow-same-origin" />
      </a>
      <div className="detail-copy"><div className="url"><code>{url}</code><button onClick={() => navigator.clipboard.writeText(url)}>Copy</button><a href={url} target="_blank" rel="noopener noreferrer">Open ↗</a></div>
        <p className="description">{artifact.description || 'No description'}</p>
        <p className="meta">Updated {date(artifact.updated_at)} by {artifact.last_publisher}<br />Created {date(artifact.created_at)} · {artifact.file_count} {artifact.file_count === 1 ? 'file' : 'files'} · {size(artifact.total_size)}</p>
        {artifact.state === 'incomplete' && <p className="warning">The last publish or delete didn't finish. Readers may see mixed files or 404s. Publish again or delete to finish.</p>}
      </div>
    </div>}
  </div>
}

function App() {
  const [artifacts, setArtifacts] = useState<Artifact[]>([])
  const [projects, setProjects] = useState<Project[]>([])
  const [base, setBase] = useState('')
  const [email, setEmail] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [query, setQuery] = useState('')
  const [incomplete, setIncomplete] = useState(false)
  const search = useRef<HTMLInputElement>(null)
  useEffect(() => {
    Promise.all([api<Artifact[]>('artifacts'), api<Project[]>('projects'), api<{ public_base_url: string }>('config'), api<{ label: string }>('whoami')])
      .then(([a, p, c, me]) => { setArtifacts(a); setProjects(p); setBase(c.public_base_url); setEmail(me.label) })
      .catch(e => setError(String(e))).finally(() => setLoading(false))
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
    <main><div className="hero"><div><span className="eyebrow">Catalogue</span><h1>Published Artifacts <span className="count">{artifacts.length}</span></h1><p className="muted">Public at {base ? new URL(base).host : 'pub.'}, never indexed or listed.</p></div><span className="muted private">♙ Descriptions are private</span></div>
      {incompleteCount > 0 && <div className="notice">{incompleteCount} {incompleteCount === 1 ? 'Artifact' : 'Artifacts'} didn't finish publishing or deleting. Publish again or delete to finish.<button onClick={() => setIncomplete(true)}>Show →</button></div>}
      <div className="toolbar"><label className="search"><span>⌕</span><input ref={search} value={query} onChange={e => setQuery(e.target.value)} placeholder="Search Projects, paths, titles, descriptions…" aria-label="Search Catalogue" /><kbd>/</kbd></label><button aria-pressed={!incomplete} onClick={() => setIncomplete(false)}>All <small>{artifacts.length}</small></button><button aria-pressed={incomplete} onClick={() => setIncomplete(true)}>Incomplete <small>{incompleteCount}</small></button><span className="spacer" /><span className="muted">{visible.length} Projects</span></div>
      {loading ? <p role="status">Loading Catalogue…</p> : error ? <p role="alert">{error}</p> : visible.length === 0 ? <p className="empty">No matching Artifacts or Projects.</p> : visible.map(g => <section className="project" key={g.project.name}><div className="project-head"><div><h2>{g.project.name} <span className="count">{g.project.artifact_count}</span></h2><code>/{g.project.name}/</code></div><p>{g.project.description}</p></div>
        {!g.artifacts.length && !g.categories.length && <p className="empty">No Artifacts yet</p>}
        {g.artifacts.map(a => <Row artifact={a} base={base} key={a.path} />)}
        {g.categories.map(c => <div key={c.name}><h3 className="category">{c.name}</h3>{c.artifacts.map(a => <Row artifact={a} base={base} key={a.path} />)}</div>)}
      </section>)}
    </main></>
}

createRoot(document.getElementById('root')!).render(<App />)
