# nginx serves Artifacts straight from S3; republish is not atomic

Artifacts are served by nginx proxying directly to an Artifact-only S3 bucket (anonymous `GetObject`, no `ListBucket`), with object keys equal to Reader paths; the Portal only writes. Republishing overwrites objects in place (assets first, entry HTML last, then delete leftovers), so a Reader hitting a Bundle mid-publish, or after a failed publish, can see a mix of old and new files until the publish completes or is retried. We accepted that window because Artifacts are short-lived, mostly viewed by their owner, and a page load straddling a republish can mix versions even with atomic replacement; in exchange Readers never depend on the Portal being up and the Portal carries no serving code, pointers or garbage collection.

## Considered Options

- **Portal streams from a private bucket, immutable per-publish prefixes + atomic pointer switch** (recommended by research, see [#4](https://github.com/yet-an-other/pub-hub/issues/4)): atomic and crash-safe, exact redirects/404s, but Portal downtime = Artifact downtime, plus serving code and a GC sweeper.
- **Portal-synced local disk served by nginx (symlink swap)**: atomic and Portal-independent, but two copies of every Artifact and a sync loop.

## Consequences

- nginx owns Reader-facing rules: `…/` → `index.html`, S3 `403` → `404`, Bundle-root trailing-slash redirect (needs a naming rule that tells single-file Artifacts from Bundles by the path alone), containment/noindex headers, `Cache-Control: no-cache`.
- The Portal sets each object's `Content-Type` at upload; S3's ETag is the validator.
- Bucket versioning stays off; deletes are immediate and final.
