# Pub Hub

A minimal, single-tenant hub for publishing short-lived static HTML pages and minisites (UI prototypes, agent-interaction pages, rendered notes) at stable, public but non-indexed URLs, with an authenticated portal to publish and browse them.

## Language

### Addressing

**Project**:
The top-level namespace an Artifact lives under (e.g. `xform`, `sicily-2025`). Exists while it holds an Artifact or carries a description; never an Artifact itself.
_Avoid_: Repo, space, folder

**Category**:
An optional, hierarchical path segment between the Project and the Artifact (e.g. `explanations`). Carries no behaviour; it only organises.
_Avoid_: Folder, tag, section

**Artifact**:
The unit of publishing: either a single `.html` file or a Bundle, addressed by Project + Categories + name. Published, replaced and deleted as a whole; Artifacts never nest, so any path belongs to at most one Artifact.
_Avoid_: Page, post, upload, asset

**Bundle**:
An Artifact that is a directory tree of static files whose entry point is `index.html` (e.g. a minisite built from Markdown).
_Avoid_: Site, archive, package

### Lifecycle

**Project deletion**:
The permanent removal of a Project's description and all its Artifacts, across every Category.
_Avoid_: Archive, hide

**Incomplete**:
An Artifact whose last publish or delete started but has not finished. Until it is published again or deleted, Readers may see a mix of old and new files, or 404s.
_Avoid_: Broken, partial, stuck

### Actors and surfaces

**Portal**:
The authenticated UI and publish API, on its own host separate from the Artifacts; it owns storage and metadata of all Artifacts.
_Avoid_: Admin, dashboard, backend

**Portal administrator**:
A person with full access to the Portal's authenticated UI, including publishing and deleting Artifacts and Projects. A machine Publisher is not a Portal administrator.
_Avoid_: Web user

**Publisher**:
Whoever publishes an Artifact: a Portal administrator or an agent/CLI with machine credentials.
_Avoid_: Author, uploader

**Reader**:
Anyone viewing a published Artifact; anonymous, no login.
_Avoid_: Visitor, guest

**Catalogue**:
The Portal's listing of all published Artifacts; visible only to authenticated users, never to Readers.
_Avoid_: Index, directory listing
