export const size = (n: number) => n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1048576).toFixed(1)} MB`

export type Artifact = { path: string; url: string; title: string; description: string; created_at: string; updated_at: string; last_publisher: string; total_size: number; file_count: number; state: string }
export type Project = { name: string; description: string; artifact_count: number }
export type Group = { project: Project; artifacts: Artifact[]; categories: { name: string; artifacts: Artifact[] }[] }

export const projectNameMatches = (expected: string, entered: string) => entered === expected

export function groups(projects: Project[], artifacts: Artifact[], query: string, incomplete: boolean): Group[] {
  const q = query.trim().toLocaleLowerCase()
  return projects.map(project => {
    const items = artifacts.filter(a => a.path.split('/')[0] === project.name && (!incomplete || a.state === 'incomplete') && (!q || [project.name, project.description, a.path, a.title, a.description, a.last_publisher].some(v => v.toLocaleLowerCase().includes(q))))
    const direct: Artifact[] = []
    const categories = new Map<string, Artifact[]>()
    for (const item of items) {
      const parts = item.path.replace(/\/$/, '').split('/')
      const category = parts.slice(1, -1).join('/')
      if (!category) direct.push(item)
      else categories.set(category, [...(categories.get(category) || []), item])
    }
    const byUpdated = (a: Artifact, b: Artifact) => b.updated_at.localeCompare(a.updated_at) || a.path.localeCompare(b.path)
    return { project, artifacts: direct.sort(byUpdated), categories: [...categories].sort(([a], [b]) => a.localeCompare(b)).map(([name, entries]) => ({ name, artifacts: entries.sort(byUpdated) })), all: items }
  }).filter(g => g.all.length > 0 || (!incomplete && (!q || [g.project.name, g.project.description].some(v => v.toLocaleLowerCase().includes(q))) && g.project.description)).sort((a, b) => {
    if (!a.all.length) return b.all.length ? 1 : a.project.name.localeCompare(b.project.name)
    if (!b.all.length) return -1
    const latest = (items: Artifact[]) => Math.max(...items.map(a => Date.parse(a.updated_at) || 0))
    return latest(b.all) - latest(a.all) || a.project.name.localeCompare(b.project.name)
  }).map(({ all: _all, ...g }) => g)
}
