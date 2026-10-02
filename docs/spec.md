# pub-hub handoff spec

This spec assembles pub-hub's design so it can be sliced into implementation issues. It restates decisions and decides nothing new: each statement cites the ticket or ADR where it was decided. Terms follow [CONTEXT.md]. Where a source was loose, the reading the spec takes is listed in §13 so the owner can overrule it. Open questions are tickets on the map, [#1], and §13.4 lists any that are open.

## 1. Overview

pub-hub is a minimal, single-tenant hub for publishing short-lived static HTML pages and minisites at stable, public but non-indexed URLs, with an authenticated Portal to publish and browse them ([CONTEXT.md]).

**Fixed constraints** ([#1]):

- Single tenant: Portal administrators and their agents publish. Readers are anonymous.
- Home server, no Docker. The Go Portal, oauth2-proxy and (later) `cloudflared` run directly under systemd.
- Manually configured nginx terminates TLS.
- Storage goes through the generic S3 API only: Ceph RGW sits behind it, but no RGW-specific features are used.
- Zitadel on the LAN is the IdP. oauth2-proxy handles the browser.
- Portal login and agent publishing are LAN/VPN-only for now. The design must not preclude opening them later.
- Backend in Go, at least 1.25 for `net/http.CrossOriginProtection` ([#2]). The Portal UI is a TS + Vite + React + Tailwind SPA embedded in the Go binary with `go:embed`, so the Portal is one deployable unit.
- Minimalism over features. Artifacts are strictly static: no Markdown rendering, no backends, no history.

**Components.** Everything runs on the RGW host and reaches RGW over loopback ([#14]).

```
Owner browser ─┐                          ┌─ /oauth2/…           → oauth2-proxy ─→ Zitadel (LAN)
               ├─→ nginx  hub.bdgn.me  ───┼─ /  and  /ui/api/…   → auth_request, then Portal
Agent / CLI ───┘   (LAN/VPN only)         ├─ /api/…              → Portal ─→ Zitadel introspection
                                          └─ /healthz, /readyz   → Portal
                                                                    │ the Portal writes both buckets
                                                                    ▼
Reader (LAN) ──────────────────→ nginx  pub.bdgn.me ── GET/HEAD ──→ RGW  pubhub-artifacts  (anonymous GetObject)
Reader (internet, later) → Cloudflare → cloudflared ─┘                   pubhub-meta       (private)
```

**Deliverables in this repo:**

| Deliverable | Where | Decided in |
|---|---|---|
| Portal: one Go binary with the embedded SPA, run as `pubhub-portal` | not fixed | [#1], [#14] |
| Naming and path-validation package shared by the Portal and the CLI | not fixed | [#10] |
| `pubhub` CLI | `cmd/pubhub` | [#10] |
| Agent skill | `skill/pubhub-publish/SKILL.md` | [#10] |
| systemd units, both nginx server blocks, example configs | `deploy/` | [#14] |
| Runbook: install, upgrade, provisioning, rollback | `docs/deploy.md` | [#14] |

**Out of scope** ([#1], [#14], [#15]):

- Backups of the buckets. They are the S3 layer's concern (§12.4).
- Artifact expiry or TTL.
- Server-side Markdown → HTML conversion.
- Per-Artifact backends or server-side state.
- Version history of republished Artifacts.
- Multi-tenant permissions or per-Project ACLs.
- Unguessable or secret URLs.
- A Prometheus `/metrics` endpoint, for now.
- How files reach the host, and TLS certificates.

## 2. URL layout and naming

### 2.1 Hosts

| Host | Serves | Reachable from |
|---|---|---|
| `hub.bdgn.me` | The Portal alone: SPA, both API prefixes, oauth2-proxy endpoints, health | LAN/VPN only: internal DNS, nginx `allow` for the LAN and VPN ranges, then `deny all` ([#14]) |
| `pub.bdgn.me` | Artifacts, at the root | The LAN now, and the internet through Cloudflare Tunnel later ([#13], [#14]) |

- The Portal and the Artifacts sit on sibling hosts. The same-site risk is accepted, and a separate registrable domain was declined ([ADR 0002], [#5]).
- The Artifact hostname is a config value, so moving to a separate domain would change URLs but not the design ([ADR 0002]).
- There is no `/pub/` prefix. `hub.` never serves Artifact bytes and has no redirect to `pub.` ([#5]).
- `s3.bdgn.me` carries no pub-hub traffic and stays off every public path, because its `/swift/info` reveals the Ceph version ([#12], [#14]).

### 2.2 Artifact URLs

- A single-file Artifact is `https://pub.bdgn.me/<project>/<category>…/<name>.html`. `.html` is the only allowed extension ([#5]).
- A Bundle is `https://pub.bdgn.me/<project>/<category>…/<name>/`, with entry point `index.html`. `…/<name>/index.html` also resolves, but `…/<name>/` is canonical ([#5]).
- Categories are optional and hierarchical: zero or more segments ([CONTEXT.md]).
- The **stem**, the path without `.html` or `/`, identifies the Artifact.
  - `plan.html` and `plan/` cannot coexist under one parent. Publishing one while the other exists is a `shape_conflict`.
  - Changing shape means deleting first and accepting a new URL ([#5], [#8]).
- There is no move or rename. Publish at the new path and delete the old one. The old URL returns 404, with no redirect ([#8]).

Examples: `pub.bdgn.me/xform/notes/plan.html`, `pub.bdgn.me/xform/roster-sync/`.

### 2.3 Naming rules

These rules cover Project, Category and Artifact names ([#5]):

- Each segment matches `^[a-z0-9]+(-[a-z0-9]+)*$`: 1 to 63 characters, no dots.
- The whole Artifact path is at most 200 characters.
- `index` and `cdn-cgi` are reserved at every level:
  - `index`, because `notes/index.html` would be served at `notes/`, and `<project>/index.html` would amount to a Project-level Artifact ([#6]).
  - `cdn-cgi`, because Cloudflare owns `/cdn-cgi/` on every proxied hostname, so an Artifact there would be unreachable ([#13], [#14]).
- The Portal rejects invalid names (`name_invalid`, `name_reserved`) and never rewrites them. Clients may slugify, but the CLI only suggests a fix ([#5], [#10]).

### 2.4 Nesting

- A path prefix is either a Category or an Artifact, never both. Publishing below an Artifact, or at a prefix that already holds Artifacts, is a `nesting_conflict` ([#5], [#8]).
- There are no Project-level Artifacts: an Artifact always has a name below its Project ([#5]).
- Projects and Categories are implicit:
  - A Category exists while it contains an Artifact.
  - A Project also exists while it carries a description ([#5], [#6]).
- `pub.bdgn.me/`, `pub.bdgn.me/<project>/` and every Category URL return 404, never a listing ([#5], [#6]).

### 2.5 Files inside a Bundle

The segment rule does not apply to files inside a Bundle ([#5]). They follow their own rules ([#8]):

- Segments are non-empty UTF-8, never `.` or `..`, with no `\` and no control characters.
- At most 255 bytes per segment and 1,024 bytes per key.
- Case is preserved, dots are allowed, and duplicates are rejected.
- Dot-files (any segment starting with `.`) are rejected by the Portal, never silently skipped. Clients skip them before upload.
- Extensionless files such as `LICENSE` or `CNAME` keep working (§5.5).

### 2.6 Search

- Every response from both hosts, error responses included, carries `X-Robots-Tag: noindex, nofollow`, sent with `always` ([#5]).
- There is no `robots.txt`, and nginx returns 404 for it, because a `Disallow` would hide the noindex header from crawlers ([#5]).
- Behind a Cloudflare Free zone, `/robots.txt` returns Cloudflare's comment-only Content Signals file instead. It has no `Disallow`, so the reasoning still holds ([#13]).
- Nothing lists Artifacts publicly. The Artifact bucket cannot be listed, and the Catalogue exists only on `hub.` ([ADR 0001], [CONTEXT.md]).

## 3. Security model

### 3.1 Threat and invariant

Artifacts are arbitrary HTML and JS. They come from the owner and their agents, but they are treated as untrusted: nothing an Artifact's script does may act with a Portal administrator's session ([#2]).

**Invariant: Artifact bytes are only ever served from the Artifact host** ([#2], [ADR 0002]).

- The Portal has no raw, download or preview route.
- It never renders Artifact HTML on its own origin: no `<iframe srcdoc>` and no `blob:` previews.
- The Catalogue does not embed Artifact previews. Its links open on the `pub.` host.

Why a sibling host ([#2], [ADR 0002]):

- On a shared origin, paths are no boundary. Artifact JS could call the API with the owner's cookie, frame Portal paths and script Portal windows.
- `CSP: sandbox` contains Artifacts only by breaking storage and workers, and one response without the header means full takeover.

### 3.2 Portal hardening (`hub.`)

- State-changing browser requests pass Go's `http.CrossOriginProtection`, which rejects the `same-site` requests that `pub.` produces ([#2], [#7]).
  - nginx forwards `Host $host`, so the check's `Origin` fallback compares correctly ([#2]).
- GET and HEAD requests have no side effects ([#2]).
- Every `hub.` response carries these headers ([#5], [#7]):
  - `Cross-Origin-Resource-Policy: same-origin`
  - `Cross-Origin-Opener-Policy: same-origin`
  - `Content-Security-Policy: frame-ancestors 'none'`
  - `X-Content-Type-Options: nosniff`
  - `X-Robots-Tag: noindex, nofollow`
- No `hub.` response ever carries a CORS header ([#7]).
- The session cookie has a `__Host-` name, `SameSite=Lax` and no `Domain`. The prefix stops `pub.` from forging or shadowing it ([#2], [#7]).
- Machine Publishers use bearer tokens, which browsers never attach on their own ([#2], [ADR 0004]).
- The Portal listens only on a unix socket that nginx alone can reach. That is why it can trust identity headers (§4.2) ([#7]).

### 3.3 Artifact host hardening (`pub.`)

- There is no auth, no oauth2-proxy and no cookie. nginx strips `Cookie` and `Authorization` before S3 and forwards only an allowlist of request headers ([#7], [#14]).
- Only GET and HEAD are allowed. Anything else gets `405` ([#14]).
- Service workers are banned, because a worker would keep serving replaced or deleted Artifacts ([#2], [#5]):
  - `Service-Worker: script` requests get `403`.
  - `Service-Worker-Allowed` is never sent.
- `nosniff` is on every response. Each object's `Content-Type` comes from a fixed extension table, set at upload ([#8], [#14]).
- `x-amz-*` and `x-rgw-*` response headers are hidden ([#12], [#14]).
- Artifact requests never fall through to anything authenticated, so Readers never reach an auth redirect ([#3], [#14]).
- A rate limit applies per Reader IP (§10.4) ([#14]).

### 3.4 Accepted residual risks

| Risk | Accepted in |
|---|---|
| `pub.` is same-site with every `*.bdgn.me` service. Artifact JS can toss `Domain=bdgn.me` cookies, including over the cookie of `kuber.bdgn.me`'s oauth2-proxy, which also allows a cookie-bomb DoS. It can also forge requests to any service that relies on SameSite alone for CSRF | [ADR 0002], [#5] |
| Same-site origins may share a renderer process. CORP and COOP mitigate Spectre-class leaks but don't exclude them | [#2] |
| An Artifact can imitate the Portal or IdP login page under `bdgn.me` | [#2] |
| All Artifacts share one origin. One can read or poison another's storage, or script another in a Reader's tab | [#2] |
| Republishing is not atomic. Readers can see a mix of old and new files during a publish, or after a failed one until it is retried | [ADR 0001] |
| Every authenticated Publisher has full rights, so an agent could delete everything. An S3-level backup is the mitigation | [ADR 0004], [#7], [#15] |

## 4. Authentication and authorisation

### 4.1 Paths on `hub.bdgn.me`

Paths from [ADR 0004] and [#7], plus the health endpoints from [#15]:

| Path | Goes to | Auth |
|---|---|---|
| `/oauth2/…` | oauth2-proxy: sign-in, callback, sign-out | `/oauth2/auth` is `internal` |
| `/api/…` | Portal, the machine Publisher API | Bearer only, no `auth_request`. nginx sets the identity headers to empty, and the Portal ignores cookies |
| `/ui/api/…` | Portal, the SPA's API | `auth_request` with the cookie, then the Portal's administrator-role check. A missing session gets a JSON `401`, and the SPA reloads |
| `/healthz`, `/readyz` | Portal, health | None (still LAN/VPN-only) |
| `/` (everything else) | Portal: SPA shell, assets, client routes | `auth_request`, then the Portal's administrator-role check. A missing session gets a `302` to `/oauth2/sign_in` |

The same Go handlers are mounted under `/api/` and `/ui/api/`, and only the authentication middleware differs, so there is one API (§6). Two prefixes exist because nginx `auth_request` cannot accept "cookie or bearer" on one path ([ADR 0004]).

### 4.2 Browser sign-in (Portal administrators)

- oauth2-proxy runs as an `auth_request` sidecar, not as a reverse proxy, so uploads never pass through it ([#3], [#7]). Use v7.15.2 or later ([#3]).
- `provider = "oidc"` against the Zitadel web app `hub-browser`: code flow, Basic auth, PKCE S256, and `offline_access` for refresh tokens ([#3], [#14], [ADR 0005]).
- The encrypted, signed cookie has a `__Host-` name and `SameSite=Lax`. It retains access and refresh tokens in a cookie with a sliding 12 h expiry and `cookie_refresh` enabled. nginx relays refresh and split-cookie headers from the auth subrequest ([ADR 0005]).
- oauth2-proxy allows any authenticated email from the configured issuer, uses `sub` for `X-Auth-Request-User`, and forwards the access token to nginx. These are identity and credential transport, not authorization. There is no owner-email file or per-person Portal config ([ADR 0005]).
- The Portal introspects the forwarded browser token with `hub-api` and requires an active token, a subject matching `X-Auth-Request-User`, and the configured `hub-admin` role in the configured project. Any authorization organization can hold the grant; `publisher` does not imply browser access. The forwarded email is only the browser Publisher label ([ADR 0005]).
- nginx sets `X-Auth-Request-User`, `X-Auth-Request-Email` and `X-Auth-Request-Access-Token` in every Portal location, from the `auth_request` values on browser paths or to empty elsewhere. A client cannot supply them directly ([ADR 0005]).
- Sign-out is `/oauth2/sign_out` ([#9]). [r-auth] §2.2 shows how to end the Zitadel session too, since a minimal session cookie keeps no ID token.

### 4.3 Machine Publishers

From [ADR 0004], [#7] and [#35]:

- **Credentials:**
  - Each agent host has its own Zitadel service account, and so does the owner's own CLI.
  - Each account holds a PAT with a one-year expiry. The owner follows this as a policy; the Portal doesn't enforce it.
- **Clients** send `Authorization: Bearer <PAT>` to `https://hub.bdgn.me/api/…` and never talk to Zitadel.
- **The Portal:**
  - It introspects each PAT through its own Zitadel API app, `hub-api`, which must belong to the configured project. It caches authorization and identity for at most 60 s, keyed by the token's hash.
  - It requires `active: true`, nonempty `sub`, and the configured `publisher` role under exactly `urn:zitadel:iam:org:project:<configured-project-id>:roles`. A nonempty grant in any authorization organization counts. The role's leaf value is an organization domain. An active PAT alone, scope, audience, unqualified role claim or matching role in another project cannot grant access ([#35], [ADR 0005]).
  - Grant or remove the role in Zitadel without changing Portal config or restarting. Effective changes depend on Zitadel propagation plus the Portal cache; removal is not guaranteed within 60 s. After cache expiry, an unavailable Zitadel yields `503` rather than stale admission.
- **Rejected** ([ADR 0004]):
  - Portal-minted API keys.
  - Client credentials or private-key JWT.
  - One prefix with header-based routing in nginx.

### 4.4 Auth outcomes

| Situation | Answer |
|---|---|
| Missing or invalid token | `401 unauthenticated` |
| Active PAT without the role in the configured project | `403 forbidden` |
| Browser session with no `hub-admin` grant in the configured project | `403 forbidden` |
| Browser token missing, inactive or with a subject different from the session | `401 unauthenticated` |
| Zitadel unreachable during introspection (transport failure) | `503 idp_unavailable`, retryable. Unexpired cached results work for at most 60 s; expired entries are never extended during an outage |
| Browser session missing or expired | `/ui/api/`: JSON `401`, and the SPA reloads. `/`: `302` to sign-in |
| Zitadel unreachable when a browser role recheck is due | `503 idp_unavailable`. If oauth2-proxy cannot refresh the token first, it may instead treat the session as expired |

Browser authorization and identity are cached for at most 60 s, or until the access token expires if sooner. Active tokens without a usable expiry claim are re-introspected on every request. Grant removal takes Zitadel propagation plus at most the cache interval; once the cache expires, an outage cannot preserve access ([ADR 0005]).

### 4.5 Authorisation and identity

- Every authenticated Publisher has full rights: publish, replace, delete and edit descriptions, anywhere. There are no per-credential scopes ([ADR 0004], [#7]).
- The **Publisher label**, recorded as "last Publisher", is introspection's current `name` for a machine Publisher, falling back to `preferred_username` and then stable `sub`; for a publish from the Catalogue it is that Portal administrator's signed-in email ([#7], [#35], [ADR 0005]). New publishes take the name after cache refresh. Existing `last_publisher` strings stay unchanged after an account rename.
- `GET /api/whoami` returns the caller's label and Portal version ([#8]).

## 5. Storage and serving

### 5.1 Buckets

There are two buckets on RGW, and both names are config values ([ADR 0001], [ADR 0003], [#14]):

| Bucket | Holds | Access |
|---|---|---|
| `pubhub-artifacts` | Artifact bytes. The object key is the Reader path without its leading `/`: `xform/notes/plan.html`, `xform/roster-sync/index.html` | Anonymous `s3:GetObject` only, no `ListBucket`. Only the Portal writes |
| `pubhub-meta` | One JSON record per Artifact, plus Project records | Private, Portal only |

- The Portal sets each object's `Content-Type` at upload, and S3's ETag is the validator ([ADR 0001]).
- Versioning is off on both buckets, so deletes are immediate and final ([ADR 0001]).
- There is no Portal-wide quota. An RGW bucket quota is a deployment choice ([#8]).
- The host keeps no state. The durable state is exactly the two buckets plus host config ([ADR 0003], [#15]).

### 5.2 Records

An Artifact's record is keyed by its stem: `xform/notes/plan.json` covers `xform/notes/plan.html` or `xform/notes/plan/`, so stem uniqueness is key uniqueness ([ADR 0003]).

| Field | Rule ([#6]) |
|---|---|
| path | The identity. The kind follows from the `.html` suffix |
| title | Taken from the entry HTML's `<title>` at each publish, falling back to the name. Never passed in |
| description | See below |
| created at | Set at the first publish and kept across republishes |
| updated at | The last publish |
| last Publisher | The Publisher label (§4.5) |
| total size, file count | Computed at publish |
| state | `incomplete` or `published` ([#17]) |

Records don't store the file list, tags or publish history ([#6]). For the file list, the Portal lists `…/<name>/` in the Artifact bucket, and the no-nesting rule guarantees everything under it belongs to that Artifact.

**Artifact description** ([#6]):

- Plain text, optional, at most 1,000 characters.
- It is private: never shown on `pub.`, and always rendered escaped.
- The Publisher sets it at publish time, and it can be edited later without republishing.
- On republish, omitting it leaves it unchanged, supplying it replaces it, and supplying an empty value clears it.
- It is never extracted from the HTML.

**Project description** ([#6]):

- It lives in an optional top-level `<project>.json` that holds only the description.
- It can be set before anything is published into the Project.
- It survives deleting the Project's last Artifact, and is removed by clearing the description.
- A described Project with no Artifacts shows in the Catalogue as empty, while `pub.bdgn.me/<project>/` stays 404.
- Categories carry no description.

### 5.3 Portal state and concurrency

- At startup the Portal lists and loads every record into memory. The metadata bucket is the Catalogue's source of truth ([ADR 0003]).
- Exactly one Portal process writes. Running two against the same buckets is unsupported ([ADR 0003]).
- One in-process lock covers the nesting checks and record writes ([ADR 0003], [#8]).
- Each Artifact has an in-memory in-flight marker ([#8]):
  - While a publish, delete or description edit of an Artifact runs, any other mutation of it gets `409 busy` with `Retry-After: 5`.
  - Different Artifacts proceed in parallel.
  - The marker is never persisted, so a record left `incomplete` after a crash blocks nothing. Publishing again or deleting fixes it.
- Project deletion atomically reserves the whole Project after checking that no Artifact or Project mutation is active. The reservation also rejects new Artifact publish, replace, delete and description edits, Project description edits, and another Project deletion with `409 busy` and `Retry-After: 5`. Upload staging is covered, even before its Artifact record exists. Reads and mutations in other Projects continue. The reservation does not hold the global metadata-write lock while deleting bytes.

### 5.4 Publish and delete sequences

**Publish (create or replace)** ([ADR 0001], [ADR 0003], [#8]):

1. Spool the whole request to a temp directory (`/var/cache/pubhub/spool`), bounded by the size cap.
2. Validate everything: names and path rules, limits, `index.html` present in a Bundle, the title extracted. An invalid upload never touches the live Artifact.
3. Write the record as `incomplete`, before the first byte.
4. Upload every file except the entry, then the entry (`index.html` or the single file).
5. Delete leftovers: files under the Bundle that are absent from the new set.
6. Mark the record `published`.

The request is synchronous and returns once the publish is complete. A crash leaves the record `incomplete`, which the Catalogue shows as Incomplete, and publishing again or deleting repairs it. No reconcile loop is needed, because a live byte never exists without a record ([ADR 0003]).

**Delete** ([ADR 0001], [ADR 0003], [#17]):

1. Write the record as `incomplete`, before touching any byte.
2. Remove the entry first, if it is still there, so the Artifact returns 404 at once.
3. Remove the other files.
4. Remove the record.

Deletion is immediate and final: a plain `404`, the name reusable at once, and no tombstones.

A crash leaves the record `incomplete`, which the Catalogue shows as Incomplete. A delete is driven by the record, not by the entry, so repeating it removes whatever bytes remain, even when the entry is already gone ([#17]).

**Project deletion** is a synchronous server-side operation. It snapshots the Project's exact-prefix Artifact records after admission, then deletes each through the lifecycle above, including Incomplete records. It stops at the first error. The Project description remains until all Artifact records and bytes are gone, then its record is removed. This operation is not atomic across Artifacts: completed deletions stay deleted after a failure, and no background cleanup or rollback runs. The Catalogue refreshes and the owner can retry after correcting the failure or restarting the Portal; retry acts on the Project's current contents. An empty undescribed Project is a successful no-op; a description-only Project loses its description. Names are reusable after deletion.

### 5.5 Serving

nginx proxies Artifact requests straight to `pubhub-artifacts` over loopback. The Portal is not on the Reader's path, so Readers don't depend on it being up ([ADR 0001], [#11], [#14]).

The `pub.` server block handles requests like this ([#5], [#11], [#14]):

- `Service-Worker: script` gets `403`.
- Methods other than GET and HEAD get `405`.
- Lookup:
  1. A path ending in `/` is looked up as `…/index.html`. Any other path is looked up as its exact key.
  2. On a miss, a path whose last segment has no dot gets a `301` that adds `/`.
     - The `Location` is relative (`absolute_redirect off`), so it never leaks the tunnel listener's port ([#13]).
     - Only this redirect path costs a second S3 round-trip.
  3. S3's `403` for a missing key becomes `404`.

### 5.6 Caching

From [#11], [#13] and [#14]:

- Every `pub.` response carries `Cache-Control: no-cache, no-transform`, sent with `always`, so `301`, `403` and `404` responses carry it too.
- The S3 ETag is the validator, and `If-None-Match` → `304` passes through.
- nginx uses no `proxy_cache`. A CDN therefore never serves stale Artifacts and needs no purge.
- nginx strips `W/` from `If-None-Match` before RGW. nginx gzip and Cloudflare compression both weaken ETags, and RGW compares them byte-wise.
- A page load that straddles a republish may mix versions. This is accepted ([ADR 0001]).

### 5.7 S3 client

From [#12] and [#14]. RGW is Ceph 19.2.3 Squid. Upgrading to 19.2.5 or later is recommended but not required.

- Use aws-sdk-go-v2 `service/s3`, built with `s3.New` and explicit options:
  - path-style addressing, region `default`;
  - `RequestChecksumCalculation` and `ResponseChecksumValidation` both set to `WhenRequired`.
- The endpoint is RGW at `127.0.0.1:<rgw port>`, bypassing `s3.bdgn.me`.
- Why the checksum pin matters:
  - With SDK defaults over HTTPS, PutObject sends `aws-chunked` with a trailing CRC32.
  - RGW 19.2.3 then stores and serves `Content-Encoding: aws-chunked` to Readers. This is fixed in 19.2.4.
  - A guard test must cover this, and it has to run over TLS, because plain HTTP does not reproduce it.
- One PutObject per file, with no multipart. A single PUT covers the 100 MB cap.
- ListObjectsV2 by prefix, paginated.
- DeleteObjects in chunks of at most 1,000 keys.
- A missing record surfaces as `*types.NoSuchKey`.
- The ranked alternative is minio-go v7 with `DisableMultipart: true`.

## 6. Publish API

### 6.1 Endpoints

The API path equals the Artifact's public path: `pub.bdgn.me/xform/notes/plan.html` ↔ `/api/artifacts/xform/notes/plan.html` ([#8]). Every endpoint is also served under `/ui/api/` for the SPA (§4.1).

| Call | Does |
|---|---|
| `PUT /api/artifacts/<path>.html` or `…/<path>/` | Publish or replace. The shape comes from the suffix |
| `GET /api/artifacts/<path>.html` or `…/<path>/` | Metadata, including the `pub.` URL |
| `PATCH /api/artifacts/<path>.html` or `…/<path>/` | `{"description": …}`, a metadata-only edit |
| `DELETE /api/artifacts/<path>.html` or `…/<path>/` | Delete, `204`. Acts whenever a record exists, even if the entry is already gone. `404` only when there is no record ([#17]) |
| `GET /api/artifacts?prefix=…` | List, with no pagination (the list is in memory and single-tenant) |
| `GET /api/projects` | Projects with their description and Artifact count, including empty described ones |
| `PATCH /api/projects/<project>` | `{"description": …}`, where an empty value clears it |
| `DELETE /api/projects/<project>` | Permanently delete every Artifact in the Project and its description; `204` only when complete. The operation is resumable by retry after partial failure |
| `GET /api/whoami` | The caller's Publisher label and Portal version (`portal_version`) |

- Paths are strict: `…/plan` without a suffix is a `404`.
- There is no version prefix. Changes stay additive.
- There is no move or rename.
- Artifact metadata, from both `GET` and the list, carries: path, `pub.` URL, title, description, created and updated times, last Publisher, total size, file count and state ([#6], [#8]).

### 6.2 Upload

From [#8]:

- Both shapes upload as `multipart/form-data`: an optional `description` text field, then one part per file. Each file part's field name is the file's path relative to the Artifact root.
- A single-file publish has exactly one file part. A Bundle must include `index.html`.
- There is no archive format, so symlinks, hard links, devices and zip-slip cannot occur.
- Paths inside a Bundle follow §2.5.
- Each stored object's `Content-Type` comes from its extension through a fixed table: html, css, js/mjs, json, svg, png, jpg, gif, webp, avif, ico, woff/woff2, txt, xml, wasm, pdf, map, and so on.
  - Unknown extensions get `application/octet-stream`.
  - No file is rejected for its type.

### 6.3 Overwrite and conditions

From [#8]:

- `PUT` answers `201` on create and `200` on replace, with the metadata and URL in the body.
- `If-None-Match: *` means create only. If the Artifact already exists, the answer is `412 exists`.
- There is no `If-Match`.
- A crashed publish or delete leaves the Artifact `incomplete`. Publishing again or deleting finishes it ([#6], [#17]).
- A create-only publish over an `incomplete` record also gets `412 exists`, because the record may still be serving files ([#17]).

### 6.4 Limits

- Each publish is capped at 100 MB for the whole request and 2,000 files per Bundle, with no per-file cap ([#8]).
- nginx `client_max_body_size` on both API prefixes sits slightly above the cap ([#8]).
- A description is at most 1,000 characters ([#6]).

### 6.5 Errors

Every error is `{"error": {"code": "…", "message": "…"}}` with a stable code ([#8]).

| Status | Codes |
|---|---|
| `400` | `name_invalid`, `name_reserved`, `path_invalid`, `request_invalid` |
| `401` / `403` | `unauthenticated`, `forbidden` |
| `404` | `not_found` |
| `409` | `shape_conflict`, `nesting_conflict`, `busy` (retryable, with `Retry-After: 5`) |
| `412` | `exists` |
| `413` | `too_large`, `too_many_files` |
| `422` | `index_missing`, `file_count` |
| `503` | `idp_unavailable`, `storage_unavailable` (retryable) |

## 7. Catalogue UI

The Catalogue is the Portal SPA at `hub.bdgn.me/`, calling the API under `/ui/api/`. It is one wide inventory list grouped by Project. The layout follows variant B of the second prototype round ([#9]; [prototype]: open `catalogue-v2.prototype.html?variant=B`), except that the public link is visible on each row and there is no preview.

### 7.1 Page

- A top bar with the pub-hub mark, "Private Catalogue", the total size and Artifact count, the signed-in administrator's email, and Sign out (`/oauth2/sign_out`).
- A heading "Published Artifacts" with the count, a **Publish** button, and the note "Descriptions are private".
- An incomplete banner when any Artifact is `incomplete`. Its "Show" link switches the filter to Incomplete.
- Search with a `/` shortcut, covering Projects, paths, titles, descriptions and Publishers, plus **All / Incomplete** filter buttons.

### 7.2 The list

- **Project header**: the name, `/<project>/`, the Artifact count, the Project description, and a small **Delete Project** action, including for empty Projects.
  - The description is click-to-edit, one line, and can be cleared.
  - An empty described Project shows "No Artifacts yet".
  - Deletion requires typing the exact Project name. Confirmation shows the total Artifact count as informational and explains that the server deletes the contents as they exist when it accepts the operation, including hidden, nested, Bundle and Incomplete Artifacts. Public URLs stop working and there is no undo.
  - Cancel makes no mutation. During deletion the UI prevents duplicate submission and refreshes after success or failure. On failure it shows the error and remaining Catalogue contents; manual Retry refreshes and reopens confirmation with the name cleared and current count. It does not retry automatically.
- **Order**: Projects by most recent activity, with empty ones last.
- **Categories**: sub-headings inside each Project. Artifacts directly under the Project come first.
- **Artifact row**: the file or Bundle icon sits left of the title. The full public URL and private description follow below, aligned with the title; the description keeps line breaks. Text shares one font family, and there is no accordion.
  - A small copy icon follows the URL. The last publish date and Artifact size sit on the right, before the action buttons.
  - An Incomplete badge and warning appear when relevant ([#17]).

### 7.3 Row actions

Small Edit, Republish and Delete buttons sit in a vertical line to the right of each Artifact (below the text and metadata on narrow screens).
- Edit opens an inline description form, with plain text up to 1,000 characters.
- Republish opens the publish form in place, with the path fixed.
- Delete asks for confirmation in the row: "Readers get 404 at once; there is no undo".

### 7.4 Publishing from the browser

- **Opening the form**:
  - Drag a file or folder onto a Project or Category heading, which highlights. The publish form opens right under that heading with the path prefilled.
  - The page's **Publish** button opens the same form at the top of the list, with an empty path.
- **The form shows** the dropped or chosen item: its kind, file count, size, `<title>`, and any dot-files skipped. Dot-files are skipped before sending ([#8]).
- **The path** is a single editable field under `pub.bdgn.me/`, with live validation, conflict detection and a replace notice.
- **Other fields**:
  - an optional description ("leave empty to keep" when replacing);
  - a "don't overwrite" checkbox, which sends `If-None-Match: *`;
  - a progress bar, since the upload returns only when the publish has finished.
- **Errors** show their API code and message. A Retry button appears for `busy` and `503`.

### 7.5 Session

- A `401` from `/ui/api/` means the browser session or access token is unusable; the SPA reloads to go through sign-in ([#7], [ADR 0005]).
- State-changing calls are same-origin, so they pass `http.CrossOriginProtection` ([#7]).

The Catalogue has no move or rename, no multi-Project bulk actions, no alternative sort orders and no activity feed ([#9], [#15]).

## 8. Publishing client: `pubhub`

### 8.1 Delivery

From [#10]:

- `cmd/pubhub` in this repo, a single static Go binary.
- It shares the naming and path-validation package with the Portal, so the client and the server can't disagree about a path.
- It is installed with `go install …/cmd/pubhub@latest`. Release binaries can come later.
- Rejected: a curl shell script, Python and Node.

### 8.2 Commands

```
pubhub publish <file|dir> <project>/<category…>/<name> [-d "description"] [--no-overwrite] [--dry-run] [--json]
pubhub describe-project <project> <description>
pubhub list [prefix] [--json]
pubhub delete <path> [--yes]
pubhub whoami
pubhub version
pubhub login
```

From [#10]:

- The target is the stem, and the source decides the shape: a file becomes `<name>.html`, a directory becomes `<name>/`. An explicit `.html` or `/` is accepted if it matches the source.
- `--no-overwrite` sends `If-None-Match: *`.
- `--dry-run` validates locally and prints what would be uploaded (count, size, `<title>`, anything skipped) without calling the API.
- `describe-project` reads `GET /api/projects` and uses `PATCH /api/projects/<project>` only if that Project has no description. It skips Projects observed with a nonempty description; this check is not atomic with the PATCH. There is no CLI command yet for editing Artifact descriptions.

### 8.3 Credentials

From [#10]:

- `PUBHUB_TOKEN` and `PUBHUB_URL` (default `https://hub.bdgn.me`) take precedence.
- Otherwise the CLI reads `$XDG_CONFIG_HOME/pubhub/config.toml`, mode `0600`, and refuses to read it if the group or others can read it.
- `pubhub login` prompts for the PAT without echo, checks it with `whoami`, then writes the file.
- There is no OS keyring. The PAT comes from Zitadel's console, one service account per host (§10.6).

### 8.4 Output and exit codes

From [#10]:

- `publish` prints only the URL on success, or full API metadata with `--json`. `list` prints a table or full API metadata with `--json`; `delete` and `describe-project` print nothing on success.
- `whoami` prints the Publisher, CLI version and Portal version on separate labelled lines. If the Portal does not return `portal_version`, it prints `unknown`. `version` prints only the CLI version without credentials or a Portal request. Release binaries report their tag; other builds use Go module build information or `dev` with the short commit and optional `-dirty` suffix.
- Progress, warnings and skipped files go to stderr.
- Errors print as `error: <code>: <message>`, using the API's codes.

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Anything else |
| `2` | Usage or local validation error |
| `3` | Conflict: `shape_conflict`, `nesting_conflict`, `exists` |
| `4` | Auth: `401`, `403` |
| `5` | Still `busy` or `503` after 3 retries, which honour `Retry-After` |

### 8.5 Client-side safety

From [#10]:

- Before any upload, the CLI validates the target, the limits (100 MB, 2,000 files) and the presence of `index.html` in a Bundle.
- It never rewrites a target. For an invalid name it suggests a fix.
- It skips dot-files and reports how many on stderr.
- It **skips symlinks and never follows them**, with a warning.
- It warns, without failing, when `index.html` references absolute `/…` paths, and suggests a relative build base (Vite `base: './'`), because a Bundle lives under a sub-path ([#2]).

## 9. Agent skill

The skill is `skill/pubhub-publish/SKILL.md` in this repo, and the owner installs it into their agents ([#10]).

- It is used **only when the user explicitly asks** to publish or share.
- **Before publishing** ([#5], [#10]):
  - It converts Markdown to a self-contained HTML page.
  - It makes sure the page has a meaningful `<title>`, which becomes the Catalogue title.
  - It builds sites with a relative base, without a service worker or PWA.
- **Path**:
  - The Project is the current git repo name, slugified.
  - The agent picks the Category and name and states the path it chose.
  - It asks only when there is no repo or the choice is unclear.
  - The naming rules apply, including the reserved names (§2.3).
- It **always passes `--no-overwrite`** unless the user said to update an existing Artifact. On `exists` or a conflict, it asks the user.
- It writes a one-line, private `-d` description saying what the page is and why it was made.
- After a successful publish, it summarizes the Project in one sentence using the root README, other docs, then source code, and runs `pubhub describe-project` to fill an empty Project description without replacing an existing one.
- It returns the URL, noting that the page is public but unlisted; if the Project description fails, it reports the separate failure.
- On an auth error, it points to `pubhub login` for missing or expired PATs, or to the machine account's role grant in the configured Zitadel project for `403`.

## 10. Deployment

Everything runs on the RGW host under systemd, with no Docker ([#14]).

- The repo ships `deploy/`, with the systemd units, both nginx server blocks and example configs.
- It also ships `docs/deploy.md`, the runbook: install; upgrade (build, copy the binary, restart); provisioning; rollback.
- How these files reach the host, by hand or through the owner's IaC, stays outside pub-hub.
- TLS certificates stay outside too: both hosts use the owner's existing `*.bdgn.me` certificate process.
- An interactive setup wizard is possible later but not planned.

### 10.1 Processes

| Unit | Runs as, listens on | Notes |
|---|---|---|
| `pubhub-portal` | User `pubhub`. `/run/pubhub/portal.sock`, mode `0660`, with nginx in its group | Spool `/var/cache/pubhub/spool`, emptied at start. `ProtectSystem=strict`, `NoNewPrivileges`. Config changes take effect on restart |
| `oauth2-proxy`, v7.15.2 or later | `/run/oauth2-proxy/o2p.sock`, mode `0660`, nginx group | **No `ExecReload`**: oauth2-proxy has no SIGHUP handler, so a reload would silently stop it. Startup needs Zitadel for OIDC discovery, so rely on `Restart=on-failure` ([#3]) |
| `cloudflared` | Added only at tunnel go-live (§11) | |

oauth2-proxy always trusts forwarded headers from unix-socket peers, so the socket's permissions are the trust boundary ([#3]).

### 10.2 Config and secrets

- **`/etc/pubhub/portal.toml`** holds the Portal's non-secret config ([#14]):
  - the bucket names and the RGW endpoint;
  - the `pub.` base URL;
  - the trusted Zitadel project ID and optional role keys, defaulting to `publisher` and `hub-admin`. Obsolete `[publishers]`, `owner_email` and `zitadel_authorization_org_id` settings fail startup with a migration error ([ADR 0005]).
- **Portal secrets** arrive via `LoadCredential=` from root-only files in `/etc/pubhub/credentials/` ([#14]):
  - the RGW keys of user `pub-hub`;
  - the `hub-api` client secret.
- **oauth2-proxy secrets**, the client secret and the cookie secret, also arrive via `LoadCredential` ([#3], [#14]). The cookie secret is raw 16, 24 or 32 bytes with no trailing newline ([#3]).
- oauth2-proxy has no email allowlist; the Portal's project-qualified `hub-admin` check grants access ([ADR 0005]).
- Every party uses the same canonical Zitadel hostname, because Zitadel derives the issuer from the `Host` header ([#3]).

### 10.3 nginx `hub.bdgn.me`

- Internal DNS only, with no public record. nginx `allow`s the LAN and VPN ranges, then `deny all` ([#14]).
- Locations as in §4.1.
  - `/oauth2/auth` is `internal`, with the request body off and `X-Forwarded-Uri` forced. This is the GHSA-7x63 mitigation ([#3]).
- Every location that proxies to the Portal forwards `Host $host` and overwrites `X-Auth-Request-User`, `-Email` and `-Access-Token` from the auth subrequest or with empty values ([#2], [ADR 0005]). Refreshed session cookies and both split-cookie parts reach the browser without dropping the security response headers.
- `client_max_body_size` sits slightly above 100 MB on `/api/` and `/ui/api/` ([#8]).
- The response headers from §3.2 go on every response.
- The nginx integration test sends forged identity and access-token headers and confirms none reaches the Portal; it checks cookie refresh propagation and response hardening headers.

### 10.4 nginx `pub.bdgn.me`

From [#13] and [#14]:

- **DNS**: internal now. The public record is added only at tunnel go-live.
- **Listeners**: `443 ssl` on the LAN. Later, a tunnel-only `127.0.0.1:8081` with no other `server` on it.
- **Proxying**: to RGW over loopback, forwarding only an allowlist of request headers: `Range`, `If-None-Match` (with `W/` stripped via a `map`) and `If-Modified-Since`.
- **Requests and headers**:
  - Request handling follows §5.5.
  - Response headers follow §2.6, §3.3 and §5.6, all sent with `always`.
  - `absolute_redirect off`.
- **Real client IP** on the tunnel listener: `set_real_ip_from 127.0.0.1` and `real_ip_header CF-Connecting-IP`. Check with `nginx -V` that the realip module is present.
- **gzip** for text types.
- **Rate limit**: a per-Reader-IP `limit_req` of about 20 r/s, burst 40.

### 10.5 RGW provisioning

From [#12] and [#14]. The plain commands go in `docs/deploy.md`.

- **Users**:
  - A provisioning-only user, `pub-hub-owner`, creates `pubhub-artifacts` and `pubhub-meta` and owns their policies and Public Access Blocks.
  - The Portal's user, `pub-hub`, gets `s3:ListBucket`, `GetObject`, `PutObject` and `DeleteObject` on both buckets, only through bucket-policy statements (`"Principal": {"AWS": "arn:aws:iam:::user/pub-hub"}`).
  - It also gets `radosgw-admin user modify --uid=pub-hub --max-buckets=-1`. Its keys then cannot change policies or Public Access Blocks, or create or delete buckets.
- **Order**:
  1. `pubhub-artifacts`: a policy holding the Portal grant and one `Allow` of `s3:GetObject` to `"Principal": "*"` on `…/*`. Optionally, a Public Access Block with only `IgnorePublicAcls: true`.
  2. `pubhub-meta`: the Portal grant first, then the Public Access Block `{BlockPublicAcls: false, IgnorePublicAcls: true, BlockPublicPolicy: true, RestrictPublicBuckets: true}`.
- **Two Squid bugs**:
  - **Never set `BlockPublicAcls: true`.** On 19.2.0–19.2.4 it makes every PutObject return `403`.
  - `BlockPublicPolicy: true` refuses every `Allow` policy on Squid. Policies therefore go first, and the flag must be lifted before a policy changes.
- **Expected result**: anonymous listing, ACL reads, PUT, DELETE and missing keys on `pubhub-artifacts` all get `403` ([#12]).

### 10.6 Zitadel provisioning

From [#3], [#7] and [#14]:

- A `pub-hub` project with "Check Role Assignment on Authentication"; grant Portal administrators `hub-admin` in this project. The Portal checks that specific role, not just whether the person has some project role ([ADR 0005]).
- Web app `hub-browser`: code flow, Basic auth, PKCE S256, callback `https://hub.bdgn.me/oauth2/callback`.
- API app `hub-api`, for the Portal's introspection.
- One service account per agent host, each with a one-year PAT. Grant `publisher` in the configured project before upgrading. Remove obsolete organization and owner-email settings, the oauth2-proxy owner-email file gate, and any `[publishers]` table. Assignments and names then change in Zitadel without a Portal restart ([#35], [ADR 0005]).

## 11. Public exposure through Cloudflare Tunnel

This applies only when `pub.` goes public ([#13], [#14]). `hub.` never appears in the tunnel.

### 11.1 Tunnel

- The tunnel is locally-managed, so its routes live in a file on the host:
  - `pub.bdgn.me` → `http://127.0.0.1:8081`, the tunnel-only listener;
  - then a catch-all `http_status:404`, which is what `hub.bdgn.me` gets.
- `cloudflared` runs under our own unit, not `cloudflared service install`:
  - `DynamicUser`;
  - the tunnel's `<UUID>.json` via `LoadCredential` (`TUNNEL_CRED_FILE=%d/…`);
  - `--no-autoupdate`, with updates through the package manager.
- `cert.pem` is account-wide and needed only to create the tunnel and its DNS route, so it never goes on the server.
- Firewall: egress TCP/UDP 7844 only, and no inbound ports.

### 11.2 Cloudflare settings (go-live checklist)

**For `pub.bdgn.me`:**

- DNS: `pub` → the tunnel, proxied. No public `hub` record points at the tunnel.
- A Cache Rule `http.host eq "pub.bdgn.me"` → **Bypass cache**. With it, every request reaches nginx, the service-worker ban always applies, and no purge is ever needed.
- Features that rewrite or inject into HTML are off: Automatic HTTPS Rewrites, Email Obfuscation, Rocket Loader, Fonts, RUM auto-injection, Markdown for Agents, and the "Add security headers" Managed Transform.

**Zone-wide, accepted knowing they also affect the other proxied `*.bdgn.me` hosts ([#14]):**

- Always Online off, since it sends URLs to the Internet Archive.
- Crawler Hints off.
- Browser Cache TTL set to "Respect Existing Headers".
- Speed Brain off.
- Replace insecure JS off.
- Bot Fight Mode off. AI Labyrinth and managed `robots.txt` are also off ([#13]).

The full checklist and the post-deployment `curl` checks are in [r-tunnel] §3 and §7.

### 11.3 After go-live

- A tunnel outage shows Readers Cloudflare error 1016 ([#15]).
- `/robots.txt` behaves as described in §2.6.

## 12. Operations

All from [#15].

### 12.1 Logging

- The Portal logs structured JSON with Go `slog` to stdout, which ends up in the systemd journal.
- **Logged**:
  - publish, replace and delete, with path, Publisher label, file count, bytes and duration;
  - description edits;
  - authentication failures (`401`/`403`), with the reason and the token's `sub` when introspection returned one;
  - `busy` and `503`;
  - a startup summary of records loaded and how many are incomplete.
- **Never logged**: tokens, cookies, request bodies.
- nginx keeps its normal access logs for both hosts. The `pub.` logs record Reader IPs, and rotation stays at the system default.
- There is no separate audit store and no activity feed in the Catalogue.

### 12.2 Health

- `GET /healthz` answers when the process is up.
- `GET /readyz` checks that both buckets are reachable and reports whether Zitadel introspection is reachable. It returns `503` only if S3 is down. Zitadel being down is reported but doesn't fail it.
- nginx exposes both on `hub.` without `auth_request`. They are still LAN/VPN-only and reveal nothing beyond up or down.

### 12.3 Monitoring

- The owner's uptime monitor checks `https://hub.bdgn.me/readyz` and one known Artifact URL on `pub.`.
- Once `pub.` is public, an external check of that URL also catches tunnel outages.
- There is no Prometheus `/metrics` endpoint for now. `cloudflared` already exposes its own metrics on `127.0.0.1:20241`–`20245`.

### 12.4 Backups and restore

- Backups are out of scope for pub-hub. Whatever backs up RGW should snapshot `pubhub-artifacts` and `pubhub-meta` from the same point in time, since records are written before bytes ([ADR 0003]).
- Host config in `/etc/pubhub/` and `/etc/oauth2-proxy/` is the rest of the durable state.
- That backup is also the mitigation for the accepted risk that an agent deletes everything ([#7]).
- A single deleted Artifact can be brought back from any copy with `pubhub publish <copy> <path>`. Only its original created time is lost.
- The spool is transient and needs no backup ([#14]).

## 13. Assembly notes

### 13.1 Superseded and refined sources

- [#4] recommended that the Portal stream from a private bucket, with an atomic pointer flip. That was declined in favour of [ADR 0001] ([#11]). The map's Decisions-so-far line for [#4] describes the recommendation, not the design.
- [#2] recommended falling back to a separate registrable domain. That fallback was declined ([ADR 0002]).
- [ADR 0003] says one lock serialises "all mutations and nesting checks". [#8] narrows that: the shared lock covers nesting checks and record writes, byte transfers for different Artifacts run in parallel, and a second mutation of the same Artifact gets `409 busy`. The spec follows [#8]. This refines the ADR rather than contradicting it.
- [ADR 0001]'s "concurrent publishes to the same Artifact are serialised by the Portal" is realised as `409 busy`, not as a queue ([#8]).

### 13.2 Readings

Where a source was loose, the spec reads it as follows. The owner can overrule any of these in review.

1. `422 file_count` is a single-file publish without exactly one file part.
2. A violation of the rules for paths inside a Bundle (§2.5) returns `400 path_invalid`. `request_invalid` covers malformed requests: bad multipart, bad JSON, an over-long description.
3. The field name of a single-file publish's one file part carries no meaning. The object key is the Artifact path.
4. `pubhub delete <path>` takes the Artifact path with its suffix, as `pubhub list` prints it and as the API requires.
5. A single-file source must be an `.html` file, since a single-file Artifact is one ([CONTEXT.md]). Anything else is a local validation error (exit `2`).
6. `portal.toml` also needs the Zitadel issuer URL and the `hub-api` client id for introspection. [#14] lists the other fields; [#35] replaced the machine allowlist, and [ADR 0005] removes the authorization organization ID and owner email in favor of project-qualified roles.
7. `/healthz` and `/readyz` are exact locations on `hub.` alongside the [ADR 0004] table ([#15]).
8. A Project's "most recent activity" is the latest `updated at` among its Artifacts.

### 13.3 Left to implementation

None of these was decided anywhere, and none is a design question:

- The JSON field names of records and API bodies.
- The exact content-type table beyond the listed types.
- Title extraction details: entities, whitespace, length.
- The CLI's human-readable `list` format.
- How the SPA learns the `pub.` base URL before any Artifact exists. Any API addition stays additive.
- The Portal's source layout beyond the paths in §1.
- nginx proxy timeouts, which must allow a synchronous publish of 100 MB or 2,000 files.

### 13.4 Open

No ticket on the map is open.

## Sources

**Map:** [#1] Map: pub-hub, minimal portal for publishing short-lived HTML Artifacts.

**Tickets:**

- [#2] Containing untrusted Artifacts on the Portal's origin (research)
- [#3] Zitadel + oauth2-proxy + nginx for browser and machine auth (research)
- [#4] Serving and atomically replacing Artifacts on generic S3 (research; superseded by [ADR 0001])
- [#5] Artifact host, URL layout and naming rules
- [#6] Artifact metadata model and store
- [#7] Authentication topology for the Portal and machine Publishers
- [#8] Publish API contract
- [#9] Catalogue UI (prototype)
- [#10] Publishing CLI and agent-skill contract
- [#11] Artifact serving path, caching and retention
- [#12] S3 client for the deployed Ceph RGW (research)
- [#13] Public exposure of pub.bdgn.me via Cloudflare Tunnel (research)
- [#14] Deployment on the home server: units, nginx, secrets, provisioning
- [#15] Backups and observability
- [#17] Crash visibility for a delete in progress
- [#35] Authorize machine Publishers through Zitadel roles

**ADRs:**

- [ADR 0001] nginx serves Artifacts straight from S3; republish is not atomic
- [ADR 0002] Artifacts on a sibling host, not the Portal's host or a separate domain
- [ADR 0003] Artifact metadata as JSON records in a private S3 bucket, held in memory
- [ADR 0004] Machine Publishers use Zitadel PATs introspected by the Portal; one API behind two auth prefixes
- [ADR 0005] Project-qualified roles govern Portal access

**Research write-ups**, on their branches: [r-containment], [r-auth], [r-s3-serving], [r-s3-client], [r-tunnel].

**Prototype:** [prototype], throwaway branch `prototype/catalogue-ui` at `268b080`.

[CONTEXT.md]: ../CONTEXT.md
[ADR 0001]: adr/0001-nginx-serves-artifacts-straight-from-s3.md
[ADR 0002]: adr/0002-artifacts-on-a-sibling-host.md
[ADR 0003]: adr/0003-artifact-metadata-in-a-private-s3-bucket.md
[ADR 0004]: adr/0004-machine-publishers-use-introspected-zitadel-pats.md
[ADR 0005]: adr/0005-project-qualified-roles-for-portal-access.md
[#1]: https://github.com/yet-an-other/pub-hub/issues/1
[#2]: https://github.com/yet-an-other/pub-hub/issues/2
[#3]: https://github.com/yet-an-other/pub-hub/issues/3
[#4]: https://github.com/yet-an-other/pub-hub/issues/4
[#5]: https://github.com/yet-an-other/pub-hub/issues/5
[#6]: https://github.com/yet-an-other/pub-hub/issues/6
[#7]: https://github.com/yet-an-other/pub-hub/issues/7
[#8]: https://github.com/yet-an-other/pub-hub/issues/8
[#9]: https://github.com/yet-an-other/pub-hub/issues/9
[#35]: https://github.com/yet-an-other/pub-hub/issues/35
[#10]: https://github.com/yet-an-other/pub-hub/issues/10
[#11]: https://github.com/yet-an-other/pub-hub/issues/11
[#12]: https://github.com/yet-an-other/pub-hub/issues/12
[#13]: https://github.com/yet-an-other/pub-hub/issues/13
[#14]: https://github.com/yet-an-other/pub-hub/issues/14
[#15]: https://github.com/yet-an-other/pub-hub/issues/15
[#17]: https://github.com/yet-an-other/pub-hub/issues/17
[r-containment]: https://github.com/yet-an-other/pub-hub/blob/research/artifact-containment/docs/research/artifact-containment.md
[r-auth]: https://github.com/yet-an-other/pub-hub/blob/research/zitadel-oauth2-proxy-auth/docs/research/zitadel-oauth2-proxy-auth.md
[r-s3-serving]: https://github.com/yet-an-other/pub-hub/blob/research/s3-serving-and-replace/docs/research/s3-serving-and-replace.md
[r-s3-client]: https://github.com/yet-an-other/pub-hub/blob/research/s3-client-for-rgw/docs/research/s3-client-for-rgw.md
[r-tunnel]: https://github.com/yet-an-other/pub-hub/blob/research/cloudflare-tunnel-exposure/docs/research/cloudflare-tunnel-exposure.md
[prototype]: https://github.com/yet-an-other/pub-hub/tree/268b080/web/prototype
