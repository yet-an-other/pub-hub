# Artifact metadata as JSON records in a private S3 bucket, held in memory

Each Artifact's metadata is one JSON record in a separate, private metadata bucket, keyed by the Artifact's stem (`notes/plan.json` for `notes/plan.html` or `notes/plan/`), plus an optional top-level `<project>.json` holding a Project description. The single Portal process loads every record into memory at startup and serialises all mutations and nesting checks with an in-process lock. We chose this over SQLite on the host because the host stays stateless, bytes and metadata back up from one S3 service, and the Portal needs no second storage technology or schema migrations; the price is that correctness relies on exactly one Portal process writing.

## Considered Options

- **No store: derive the Catalogue by listing the Artifact bucket**, title and Publisher in S3 user metadata: a crashed Bundle publish leaves inner `.html` files that list as phantom single-file Artifacts, and user metadata is readable by anyone reaching RGW directly.
- **SQLite on the host**: transactional checks, but host state to back up, migrations, and drift against the bucket.

## Consequences

- **Record first.** Both publishing and deleting write the record with `state: incomplete` before touching any byte. Publishing sets `published` after leftovers are deleted; deleting removes bytes (entry first) and then the record. A live byte never exists without a record, so a crashed publish or delete shows as Incomplete in the Catalogue and no reconcile loop is needed (amended by [#17](https://github.com/yet-an-other/pub-hub/issues/17)).
- Running two Portal processes against the same buckets is unsupported.
- The metadata bucket is a second backup target alongside the Artifact bucket.
