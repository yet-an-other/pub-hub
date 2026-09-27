# Serving and atomically replacing Artifacts on generic S3

Research for [#4](https://github.com/yet-an-other/pub-hub/issues/4) (part of the map, #1). Researched 2026-09-27 against primary sources: the AWS S3 API reference and user guide, Ceph RGW docs and source, nginx.org module docs, Go stdlib and SDK package docs. Sources are listed [at the end](#sources); inline references look like [S3-consistency].

This document records facts and a recommendation. It does not decide the metadata model (#6), URL layout (#5) or containment headers (#2). Where the answer depends on those tickets, it says so.

## Recommendation in brief

- **Serving path:** nginx proxies `/pub/…` to the Go Portal, and the Portal streams each file from a **private** bucket. For every Reader request, the Portal resolves the path to an Artifact using an in-memory route table, then maps it to the Artifact's current publish and an object key. The Portal sets every response header itself: `Content-Type`, `X-Robots-Tag`, the CSP from #2, `Cache-Control` and `ETag`. It also handles `index.html` resolution, trailing-slash redirects and 404s. Range and conditional requests go through Go's `http.ServeContent`. S3 credentials exist only in the Portal, and the S3 endpoint is never on a public path.
- **Replace mechanism:** each publish uploads into its own **immutable prefix** (`<publish-id>/…`). Once the upload is complete, one atomic step moves the Artifact's **pointer** to that prefix: a single-row transaction or a single-key PUT, depending on #6. Superseded prefixes are garbage-collected after a grace period. A sweeper removes orphaned prefixes left by crashed or aborted publishes.
- **Main trade-off:** the Portal sits in the Reader hot path, so Artifacts go down when the Portal is down. It also needs a few hundred lines of Go. In return it is the only option that satisfies atomic Bundle replacement, full header control and credential isolation together, using only the generic S3 API.

## 1. What generic S3 does and does not give us

These facts constrain every option. Where RGW differs or its behaviour is unverified, that is noted.

| Fact | Consequence for pub-hub | Source |
|---|---|---|
| General purpose buckets "have a flat structure instead of a hierarchy". "Folders" are just shared key prefixes. | S3 has no directories, so a request for `…/sicily-2025/` does not map to an object. Something must translate `/` to `/index.html`. | [S3-folders] |
| "Updates to a single key are atomic… you will get either the old data or the new data, but never partial or corrupt data." | A single-file Artifact could be overwritten in place, and a reader would never see a torn file. | [S3-consistency] |
| "Updates are key-based. There is no way to make atomic updates across keys." | Overwriting a Bundle's objects in place is **not** atomic. Readers can see a mix of old and new files, or files that are missing. Some form of indirection is required. | [S3-consistency] |
| General purpose buckets have no rename. `RenameObject` "is only supported for objects stored in the S3 Express One Zone storage class". | "Upload to staging, then rename into place" is unavailable. A move would be copy plus delete, per object, and not atomic. | [S3-RenameObject] |
| AWS: "strong read-after-write consistency for PUT and DELETE requests", including overwrites and LIST. | This is true on AWS, but I found no equivalent documented guarantee for RGW. The recommended design only ever reads **new, never-overwritten keys** through the serving path, so it does not depend on overwrite consistency (see §3). | [S3-consistency] |
| Concurrent writers: "the request with the latest timestamp wins… you must build an object-locking mechanism into your application." | Two concurrent publishes of the same Artifact need serialising in the Portal, or compare-and-swap on the pointer. | [S3-consistency] |
| Conditional writes: `If-None-Match: *` (create only) and `If-Match: <etag>` (compare-and-swap) on PutObject, CompleteMultipartUpload and CopyObject return **412** on failure, or 409 on some races. | These give a CAS primitive if the pointer lives in S3. | [S3-condwrites] |
| RGW parses `If-Match`/`If-None-Match` in `RGWPutObj_ObjStore_S3::get_params` on main and on the quincy, reef, squid and tentacle branches. Ceph calls the feature "supported since 2014", but it had bugs on **versioned buckets** and **multipart uploads**. Those were fixed in PR 63348 (merged 2025-07) and backported to squid and tentacle. | Conditional PUT works on RGW for plain single-PUT objects in unversioned buckets. Do not build on it without testing the deployed version. | [Ceph-src-put], [Ceph-68183], [Ceph-tentacle] |
| User-defined metadata (`x-amz-meta-*`) is "limited to 2 KB" in total (UTF-8 bytes of keys plus values). It **cannot be modified after upload**: "the only way to modify this metadata is to make a copy of the object". RGW documents "a string up to 8kb", but 2 KB is the generic floor. | Object metadata cannot serve as a mutable pointer. It is fine for small immutable per-object facts such as a content hash. | [S3-metadata], [Ceph-objectops] |
| Settable system metadata that S3 echoes on GET: `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Type`, `Expires` (plus `Content-Language` via response overrides). | S3 **cannot store or emit** `X-Robots-Tag`, `Content-Security-Policy` or other arbitrary headers. They must be added by nginx or the Portal on every path. | [S3-metadata], [S3-GetObject] |
| `response-content-type` and similar overrides "cannot be used with an unsigned (anonymous) request". | A public-read bucket cannot fix headers per request. | [S3-GetObject] |
| A GetObject for a missing key returns **403**, not 404, when the requester lacks `s3:ListBucket`. | With an anonymous public-read bucket (no ListBucket), a proxy sees 403 for "not found" and must remap it. Granting anonymous ListBucket would publish a listing of every Artifact, which is forbidden. | [S3-GetObject] |
| `Range`: "Amazon S3 doesn't support retrieving multiple ranges of data per GET request." `If-None-Match` and `If-Modified-Since` produce a 304. | Single-range requests and conditional GETs pass through. Multi-range requests do not. | [S3-GetObject] |
| ETag "is an MD5 digest of the data" only for non-multipart, unencrypted or SSE-S3 objects. | An S3 ETag is not a content hash in general. The Portal can mint its own ETag from a hash it records at publish time. | [S3-metadata] |
| ListObjectsV2 returns at most 1,000 keys per page, in lexicographic order for general purpose buckets. DeleteObjects takes up to 1,000 keys per request, and Content-MD5 is required. | Cleanup of a prefix is list-then-batch-delete, paginated. | [S3-ListObjectsV2], [S3-DeleteObjects] |
| On a versioned bucket, delete only adds a delete marker and all versions remain. | **Leave bucket versioning off.** It would keep exactly the history the project rules out, and makes deletes non-final. | [S3-DeleteObjects] |
| Website endpoints (index documents, HTML errors) are GET/HEAD only, public content only, and have no TLS on AWS. On RGW the website API is `rgw_enable_static_website` (default `false`) and needs dedicated website hostnames. | This is a deployment-specific feature, not the generic REST API. It is excluded by the constraints. | [S3-website], [Ceph-rgw-opts] |

## 2. Serving path options

### A. nginx `proxy_pass` straight to the bucket

The bucket is either **public-read**, with anonymous GetObject granted by policy, or read with **nginx-signed** requests.

- **Signing:** nginx has no built-in SigV4. NGINX's own reference gateway implements signing in **njs** (`awssig4.js`), a JavaScript module added to nginx [nginx-s3-gateway]. Signed mode therefore means adding njs plus that code, with S3 keys in nginx's environment.
- **Atomic Bundle replace: not achievable by nginx alone.** Stable Reader URLs must map to the *current* publish, and nginx has no way to read a pointer. The workarounds all bring an application back into the path:
  - A Portal-generated `map` include plus `nginx -s reload` on every publish. The Portal would need privileges to rewrite nginx config and reload it.
  - An `auth_request` subrequest to the Portal whose response header names the current prefix [nginx-auth_request]. This is one Portal round-trip per request anyway.
  - Portal-issued `X-Accel-Redirect` to an `internal` location [nginx-core#internal], [nginx-proxy#proxy_ignore_headers].
- **`index.html` and trailing slash:** nginx's `index` and `try_files` work on the local filesystem only [nginx-index], [nginx-core#try_files]. Against S3 you need `rewrite ^(.*)/$ $1/index.html` plus a rule to add the slash to Bundle roots. nginx cannot tell from `/pub/sicily-2025` alone whether it names a Bundle or nothing, so it falls back on a naming heuristic. The NGINX gateway treats a path as a possible directory when its last segment contains no dot [nginx-s3-gateway-docs]. That ties this option to naming rules in #5.
- **404:** set `proxy_intercept_errors on` [nginx-proxy#proxy_intercept_errors] with `error_page 403 404 =404 /404.html;` to turn S3's 403 into a clean 404. Anonymous ListBucket must stay off.
- **Headers:** `add_header … always` for noindex and CSP [nginx-headers]. Beware that `add_header` directives "are inherited from the previous configuration level if and only if there are no `add_header` directives defined on the current level". `add_header_inherit merge` fixes this, but only from nginx **1.29.3** onward. Use `proxy_hide_header` for `x-amz-*`, `x-rgw-*` and `x-amz-meta-*` [nginx-proxy#proxy_hide_header]. `Content-Type` comes from the object metadata set at upload.
- **Caching and ranges:** without `proxy_cache`, `Range` and `If-*` headers pass through to S3. With `proxy_cache`, nginx does not pass them upstream [nginx-proxy#proxy_set_header], and `proxy_cache_purge` requires the commercial subscription [nginx-proxy#proxy_cache_purge].
- **Credentials and exposure:** a public-read bucket makes **every object readable by anyone who can reach RGW**. That includes superseded or staged prefixes and any pointer or manifest objects. The client path also maps directly onto an object key. Signed mode avoids that, but puts keys into nginx.

### B. Go Portal streams from S3 (nginx → Portal → S3)

nginx terminates TLS and proxies `/pub/` to the Portal on localhost. For each request, the Portal:

1. Resolves the path to an Artifact and its current publish ID, using an in-memory table loaded from the metadata store (#6). The Portal is the only writer, so this cache is coherent without relying on S3 overwrite consistency.
2. Handles the path before touching S3:
   - It issues a 301 from a Bundle root without its trailing slash, and from subdirectories whose `index.html` exists.
   - It maps a path ending in `/` to `index.html`.
   - It returns its own 404 for anything not in the publish's file list. No S3 round-trip is needed when the file list (manifest) is known.
3. Sets headers itself:
   - `Content-Type` chosen at publish time, so it does not come from sniffing or from the client.
   - `X-Robots-Tag: noindex`, the containment CSP and `X-Content-Type-Options` (per #2).
   - `Cache-Control`.
   - A strong `ETag` from the manifest's content hash, which lets it answer `If-None-Match` with a 304 without calling S3.
4. Streams the body from S3. Go's `http.ServeContent` "handles Range requests properly… and handles If-Match, If-Unmodified-Since, If-None-Match, If-Modified-Since, and If-Range" if given an `io.ReadSeeker` [go-ServeContent]. minio-go's `*minio.Object` "implements Reader, ReaderAt, Seeker, Closer for a HTTP stream" [minio-Object], so it plugs in directly. With aws-sdk-go-v2, pass the client's `Range` to `GetObject` and relay the 206 instead. Set `Content-Type` before calling `ServeContent`, or it will guess from the extension or content [go-ServeContent].

Trade-offs:

- **Pros:**
  - It is the only option where atomic replace, directory semantics, 404s and headers are all enforced in one place with no heuristics.
  - The bucket stays private, and only keys the Portal computed from its own manifest are ever fetched, so no Reader path can reach staging or superseded objects.
  - The nginx config shrinks to a single `location`, which avoids `add_header` inheritance traps.
- **Cons:**
  - **Artifact availability equals Portal availability.** A Portal restart means brief 502s for Readers.
  - Every byte flows through Go, which is irrelevant at home-server scale for small static files.
  - Go code must be written and tested.
  - `proxy_cache` in nginx could mask Portal restarts via `proxy_cache_use_stale`, but it delays republish and delete visibility and has no free purge. I would not add it in v1.
- **SDK gotcha:** aws-sdk-go-v2 `service/s3` v1.73.0 (2025-01-15) and later "default to enabling an additional checksum on all Put calls and enabling validation on Get calls" (CRC32). The announcement warns that third-party S3 implementations may not support this [aws-go-2960], [aws-sdk-integrity]. Against RGW, set `RequestChecksumCalculation`/`ResponseChecksumValidation` to `when_required` [aws-go-Options], or use minio-go. Verify either choice against the deployed Ceph version.

### C. Portal-synced local filesystem, nginx serves statically

On publish, the Portal writes to S3 (the source of truth) **and** materialises the publish under `/srv/pub-hub/versions/<publish-id>/`. It then points a per-Artifact **symlink** at it with create-temp-link plus `rename(2)`. "If newpath already exists, it will be atomically replaced, so that there is no point at which another process attempting to access newpath will find it missing" [rename2]. `rename` cannot replace a non-empty directory, which is why the swap goes through a symlink.

- **Pros:**
  - nginx natively provides `index`, `try_files`, the directory-to-slash redirect, `etag on` by default [nginx-core#etag], byte ranges and `sendfile`.
  - Readers keep working when the Portal is down.
  - No S3 traffic on reads.
- **Cons:**
  - **Two copies of every Artifact**, plus reconcile code for boot and drift, and more disk.
  - It arguably bends the "storage via S3" constraint, since S3 becomes a backup.
  - `Content-Type` comes from nginx's extension map, not from per-Artifact decisions.
  - nginx follows symlinks by default (`disable_symlinks off`) [nginx-core#disable_symlinks]. Bundle extraction must therefore reject symlinks and path traversal, or nginx serves whatever they point to. Use `disable_symlinks if_not_owner` as a backstop.
  - `open_file_cache` (default `off`) must stay off, or be kept short, so swaps are visible [nginx-core#open_file_cache].

### Comparison

| Concern | A. nginx → S3 | B. Portal streams | C. Local FS mirror |
|---|---|---|---|
| Atomic Bundle replace | ✗ without an app in the path | ✓ pointer flip | ✓ symlink `rename(2)` |
| Header control (noindex, CSP, Content-Type) | nginx `add_header` + upload-time metadata | ✓ full, per Artifact | nginx `add_header` + mime map |
| `/` → `index.html`, slash redirects | rewrite + naming heuristic | ✓ exact (manifest) | ✓ native |
| Correct 404 | remap S3 403 → 404 | ✓ | ✓ native |
| ETag / 304 | S3 ETag passthrough | ✓ manifest hash, 304 without S3 | ✓ native |
| Range | single range passthrough | ✓ via `ServeContent` / Range passthrough | ✓ native |
| Credentials off public path | ✗ public bucket, or keys in njs | ✓ Portal only | ✓ Portal only |
| Reader availability if Portal down | ✓ | ✗ | ✓ |
| Moving parts | njs or public bucket + heuristics | Go handler | sync/reconcile + two stores |

## 3. Atomic replacement of a Bundle

**Mechanism: immutable publish prefixes plus a pointer.**

1. **Stage.** The Portal receives the whole Artifact, validates and extracts it, and mints a fresh `publish-id` (for example a ULID). It uploads every file to `<publish-id>/<relpath>` with its `Content-Type` set. These keys are new and never overwritten. Optionally add `If-None-Match: *` to assert that [S3-condwrites].
2. **Record.** The Portal writes the publish's manifest: file list, sizes, content hashes and content types. Where the manifest lives is a #6 decision (§5).
3. **Commit.** A single atomic step flips `Artifact → publish-id`. That is one DB transaction, or one PUT of a pointer object (single-key atomic [S3-consistency]), optionally a CAS with `If-Match: <old-etag>` [S3-condwrites]. Publishes to the same Artifact are serialised inside the Portal, for example with a per-Artifact mutex; the Portal is a single process.
4. **Activate.** The Portal updates its in-memory route table. From then on, new Reader requests resolve to the new prefix.
5. **Retire.** The old `publish-id` is queued for GC after a **grace period** (minutes to hours). Requests already in flight finish against intact objects. GC lists the prefix (≤1,000 keys/page [S3-ListObjectsV2]) and batch-deletes it (≤1,000 keys/request [S3-DeleteObjects]).

**What this guarantees:**

- Every response comes from exactly one fully uploaded, committed publish.
- A failed or partial upload is never visible, because nothing points at it.
- The serving path only reads keys written once before the commit, and the pointer is read from Portal memory. Correctness therefore does not depend on S3 overwrite or list consistency, which matters because RGW's consistency is less explicitly documented than AWS's (§1).

**What it does not guarantee, in any option:** consistency *across* the several requests of one page load. If a Reader fetches `index.html` just before the flip and `site.css` just after, they get HTML from v1 and CSS from v2. Every file comes from a complete publish, but the page can still be mixed. Pinning a Reader to a version would need versioned URLs, which break the stable, human-readable URLs, or a cookie, which conflicts with CDN caching. Neither seems worth it for short-lived pages. The practical mitigation is caching policy:

- Serve Artifacts with `Cache-Control: no-cache` plus a strong `ETag`. Browsers revalidate each file cheaply (304 from the manifest hash) and never mix a *cached* old asset with new HTML for longer than one page load.
- Behind Cloudflare, "The Cloudflare CDN does not cache HTML or JSON by default" but does cache CSS, JS and images by default. It does **not** cache responses whose `Cache-Control` is `private`, `no-store`, `no-cache` or `max-age=0` [cf-cache]. Without such a header, the edge would serve new HTML with stale CSS or JS after a republish. So either send `no-cache` (no edge caching) or purge on every publish and delete.

**Single-file Artifacts** could skip the indirection, because a single-key overwrite is atomic [S3-consistency]. Using the same prefix-and-pointer path anyway keeps one code path. It also makes a file-to-Bundle change at the same name trivial, and gives GC one shape.

**Failure and crash cases:**

- **Crash before commit:** an orphan prefix with no pointer. A periodic sweeper deletes prefixes that are unreferenced and older than N hours.
- **Crash after commit, before GC:** the old prefix stays unreferenced, and the same sweeper collects it.
- **Interrupted multipart uploads (large files):** add a bucket lifecycle rule with `AbortIncompleteMultipartUpload`, as AWS recommends [S3-condwrites]. RGW lists Bucket Lifecycle as supported [Ceph-S3]. Lifecycle cannot express "unreferenced", so it only backs up the sweeper.

## 4. Deletion semantics and cleanup

- **Delete = remove the pointer (commit), then drop it from the route table.** Readers get 404 immediately. Object deletion follows asynchronously, using the same GC as republish: list the prefix, then DeleteObjects in batches. DeleteObjects reports a missing key as deleted [S3-DeleteObjects], so retrying GC is idempotent.
- **No tombstones.** "No history" means a deleted path returns a plain 404, not 410, and the name can be reused immediately by a new publish with a new prefix. Returning 410 would require remembering deleted paths.
- **Ordering:** pointer first, objects later. The reverse order would serve 404s for files of a still-"published" Artifact.
- **CDN (future):** a deletion or republish must also purge the Artifact's URLs at the edge, unless responses are `no-cache` (§3).
- **Bucket versioning stays off.** Otherwise deletes leave every version behind [S3-DeleteObjects].

## 5. Where the pointer and metadata can live (facts for #6)

| Place | What it can hold | Atomicity / concurrency | Limits and costs |
|---|---|---|---|
| **Object user metadata** (`x-amz-meta-*`) | Small immutable per-object facts, such as a hash | Set only at upload. Changing it means a copy. | ≤ 2 KB total generic (RGW doc: 8 KB). Returned as response headers, so a proxy must hide it. [S3-metadata], [Ceph-objectops] |
| **Object tags** | ≤ 10 key/value tags | Separate API | Usable in lifecycle rules. Too small for a manifest. [S3-metadata] |
| **Manifest object per publish** (`<publish-id>/manifest.json`) | File list, hashes, types, title, publisher, and so on | Write-once, so no races | Not a pointer by itself, but written last it acts as the publish's commit marker. It makes the bucket self-describing, so the Catalogue can be rebuilt from it. |
| **Pointer object per Artifact** (small JSON at a stable key) | `publish-id` and summary metadata | Single-key atomic overwrite. Last writer wins unless `If-Match` CAS is used (AWS; RGW with version caveats). [S3-consistency], [S3-condwrites], [Ceph-68183] | Catalogue = ListObjectsV2 over the pointer prefix (1,000/page). Nesting/collision checks across Artifacts are not atomic; the Portal must serialise them. |
| **Single catalogue object** (all Artifacts in one JSON) | Everything | Atomic whole-catalogue swap, with CAS for safety | Rewritten on every publish. Fine for hundreds of Artifacts. |
| **External store on the host** (for example SQLite) | Everything, queryable | Multi-row transactions. Collision and nesting checks are atomic with the flip. | A second thing to back up. If lost, it can be rebuilt only if the bucket is self-describing (manifests, and ideally human-readable key prefixes). |

Two layout facts are relevant to #6:

- Keys named only by `publish-id` keep storage independent of the Reader path, so a rename or move is a pointer change.
- Keys prefixed with the Reader path (`<project>/<categories>/<name>/<publish-id>/…`) make the bucket human-browsable and easier to recover without the metadata store.

Either works with the mechanism in §3.

## 6. Recommendation and trade-offs

**Serving path: option B, the Portal streams from a private bucket.**

- nginx: `location /pub/ { proxy_pass http://127.0.0.1:<port>; }`, plus TLS. The S3 vhost stays LAN-only and must never be routed through a future Cloudflare Tunnel.
- The Portal owns path resolution, `index.html`, slash redirects, 404s, `Content-Type`, noindex, CSP, `Cache-Control: no-cache` and manifest-hash `ETag`s. It uses `http.ServeContent` for Range and conditional requests.
- Why not A: it cannot replace Bundles atomically without putting an app back in the path. It also needs 403-to-404 remapping and naming heuristics for directories, and either exposes the whole bucket or embeds keys and signing code in nginx.
- Why not C (for now): it doubles storage, adds a sync and reconcile loop, and weakens the "S3 is storage" constraint. Its main advantage, Readers surviving Portal downtime, is worth revisiting only if Reader availability starts to matter more than minimalism, for example after public exposure. The replace mechanism below carries over unchanged, since C is just a local materialisation of the same immutable prefixes.

**Replace mechanism: immutable per-publish prefixes, an atomic pointer flip, and deferred GC with an orphan sweeper** (§3–4). The pointer's home is #6's call. SQLite on the host is the only listed option that makes the collision/nesting check and the flip one transaction. An S3 pointer object needs no extra store, but relies on in-Portal serialisation, or on RGW conditional writes if more than one writer ever exists.

**Accepted costs:**

- Portal downtime means Artifact downtime.
- A small Go handler is needed.
- Page loads can mix versions across requests during the instant of a republish, which is mitigated by `no-cache` and ETags.
- Superseded publishes linger for the GC grace period. This is not user-visible history, but it is storage.

## 7. To verify in a prototype

- **The S3 client against the deployed RGW version:**
  - minio-go `Object` with `ServeContent`: how many S3 requests per Reader request. `ServeContent` seeks to the end to learn the size, and minio-go's first `Seek` issues its own request to fetch object info (`api-get-object.go`, v7.3.0).
  - aws-sdk-go-v2 with `when_required` checksums.
  - `If-None-Match: *` and `If-Match` returning 412 as documented.
  - Whether an anonymous or unauthorised missing-key GET returns 403 or 404 on RGW. This only matters for option A.
- **Bundle crosslinks:** relative links in `index.html` resolve correctly after the root 301 to `/`. Without the slash, relative references resolve against the parent path ([RFC3986] §5.2.3), which is why the redirect is mandatory.

## Sources

- [S3-consistency] AWS, *What is Amazon S3? — Amazon S3 data consistency model*: https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html#ConsistencyModel
- [S3-metadata] AWS, *Working with object metadata*: https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingMetadata.html
- [S3-condwrites] AWS, *How to prevent object overwrites with conditional writes*: https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html
- [S3-GetObject] AWS API Reference, *GetObject*: https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html
- [S3-DeleteObjects] AWS API Reference, *DeleteObjects*: https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html
- [S3-ListObjectsV2] AWS API Reference, *ListObjectsV2*: https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html
- [S3-RenameObject] AWS API Reference, *RenameObject*: https://docs.aws.amazon.com/AmazonS3/latest/API/API_RenameObject.html
- [S3-folders] AWS, *Organizing objects in the Amazon S3 console by using folders*: https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-folders.html
- [S3-website] AWS, *Website endpoints*: https://docs.aws.amazon.com/AmazonS3/latest/userguide/WebsiteEndpoints.html
- [Ceph-S3] Ceph, *Ceph Object Gateway S3 API — Features Support*: https://docs.ceph.com/en/latest/radosgw/s3/
- [Ceph-objectops] Ceph, *Object Operations* (PUT `x-amz-meta-*` "up to 8kb"): https://docs.ceph.com/en/latest/radosgw/s3/objectops/
- [Ceph-src-put] Ceph source, `RGWPutObj_ObjStore_S3::get_params` reads `HTTP_IF_MATCH`/`HTTP_IF_NONE_MATCH`: https://github.com/ceph/ceph/blob/main/src/rgw/rgw_rest_s3.cc (same on the `quincy`, `reef`, `squid` and `tentacle` branches)
- [Ceph-68183] Ceph tracker #68183, *rgw: conditional write doesn't work in certain scenarios*: https://tracker.ceph.com/issues/68183; fix PR https://github.com/ceph/ceph/pull/63348
- [Ceph-tentacle] Ceph Tentacle release notes ("fix conditional Delete, MultiDelete and Put"): https://docs.ceph.com/en/latest/releases/tentacle/
- [Ceph-rgw-opts] Ceph `rgw_enable_static_website` / `rgw_dns_s3website_name`: https://github.com/ceph/ceph/blob/main/src/common/options/rgw.yaml.in
- [nginx-proxy] nginx, *ngx_http_proxy_module* (`proxy_pass`, `proxy_intercept_errors`, `proxy_hide_header`, `proxy_set_header`, `proxy_ignore_headers`, `proxy_cache_purge`): https://nginx.org/en/docs/http/ngx_http_proxy_module.html
- [nginx-headers] nginx, *ngx_http_headers_module* (`add_header`, `add_header_inherit`): https://nginx.org/en/docs/http/ngx_http_headers_module.html
- [nginx-core] nginx, *ngx_http_core_module* (`location` slash redirect, `try_files`, `internal`, `etag`, `open_file_cache`, `disable_symlinks`): https://nginx.org/en/docs/http/ngx_http_core_module.html
- [nginx-index] nginx, *ngx_http_index_module*: https://nginx.org/en/docs/http/ngx_http_index_module.html
- [nginx-auth_request] nginx, *ngx_http_auth_request_module*: https://nginx.org/en/docs/http/ngx_http_auth_request_module.html
- [nginx-s3-gateway] NGINX, *nginx-s3-gateway* (njs `awssig4.js`): https://github.com/nginxinc/nginx-s3-gateway; [nginx-s3-gateway-docs] https://github.com/nginxinc/nginx-s3-gateway/blob/main/docs/getting_started.md
- [go-ServeContent] Go, `net/http.ServeContent`: https://pkg.go.dev/net/http#ServeContent
- [minio-Object] minio-go v7, `Object`: https://pkg.go.dev/github.com/minio/minio-go/v7#Object
- [aws-go-Options] aws-sdk-go-v2 `service/s3.Options` (`RequestChecksumCalculation`, `ResponseChecksumValidation`, `UsePathStyle`) and `PutObjectInput` (`IfMatch`, `IfNoneMatch`): https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/s3
- [aws-go-2960] aws-sdk-go-v2, *Announcement: S3 default integrity change*: https://github.com/aws/aws-sdk-go-v2/discussions/2960
- [aws-sdk-integrity] AWS SDKs and Tools Reference, *Data Integrity Protections for Amazon S3*: https://docs.aws.amazon.com/sdkref/latest/guide/feature-dataintegrity.html
- [rename2] Linux man-pages, *rename(2)*: https://man7.org/linux/man-pages/man2/rename.2.html
- [cf-cache] Cloudflare, *Default cache behavior*: https://developers.cloudflare.com/cache/concepts/default-cache-behavior/
- [RFC3986] RFC 3986 §5.2.3 *Merge Paths*: https://www.rfc-editor.org/rfc/rfc3986#section-5.2.3
