---
name: pubhub-publish
description: Publish or share an HTML page, Markdown note, or static site through pubhub when the user explicitly asks to publish or share it. Do not use for drafting, previewing, or building alone.
---

# Publish with pubhub

Use this skill only after the user explicitly asks to publish or share. Publishing makes the Artifact publicly reachable by anyone with its URL. The URL is unlisted, not private or access-controlled. Do not publish secrets.

## Prepare the Artifact

- For Markdown, render it to a **self-contained `.html` file** before calling `pubhub`: include the content and styling in the file, embed local images and other required assets rather than leaving local filesystem references or relying on a remote CDN. Escape untrusted Markdown/HTML appropriately. Keep ordinary outbound links if they are part of the note.
- Check the HTML has a specific, meaningful `<title>` in `<head>`; the Portal uses it as the Catalogue title. Do not rely on the CLI's fallback to the Artifact name. For a Bundle, check its root `index.html`.
- Build a static site as a directory with `index.html`, a relative base for asset URLs (for example, Vite `base: './'`), and no service worker or PWA output. Check that assets resolve from the Artifact's sub-path, not `/`. Remove any previously generated service worker, manifest or registration code before publishing.
- A single-file source must end in `.html`. The CLI skips dot-files and symlinks in Bundles, so check that no required content depends on them. The CLI validates a maximum of 100 MB per request and 2,000 files.

## Choose the path

1. Find the current git repository's top-level directory with `git rev-parse --show-toplevel` and slugify its **directory name** for the Project. Do not use the current subdirectory or the remote's name. If there is no git repository, ask the user for a Project rather than inventing one.
2. Pick a Category (optional, possibly multiple segments) and an Artifact name that describe the content. If the intended Category or name is unclear, ask. Otherwise choose them and **state the full path you chose** before publishing, for example `pub-hub/notes/release-plan.html`.
3. Use lowercase ASCII letters and digits with single hyphens between words. Every Project, Category and Artifact segment must match `^[a-z0-9]+(-[a-z0-9]+)*$`, be 1 to 63 characters long, and must not be `index` or `cdn-cgi`. The full path, including `.html` or `/`, must be at most 200 characters. Do not choose a Project-level Artifact: include a name after the Project. If slugifying the repo directory produces an empty, reserved, or otherwise invalid Project, ask for a valid one.
4. Supply the path as a stem (`project/category/name`). The CLI adds `.html` for a file and `/` for a directory. You may supply that suffix explicitly if it matches the source. The stem cannot switch an existing Artifact between file and Bundle shapes without deleting it first.

## Publish

Write a one-line, **private** description for `-d` that says what the Artifact is and why it was made. It is not a substitute for the HTML title. Keep it under 1,000 characters. Avoid credentials or secrets even in private metadata.

For a new Artifact, always run:

```sh
pubhub publish <prepared.html|site-dir> <project/category/name> -d "<what it is and why it was made>" --no-overwrite
```

Only omit `--no-overwrite` if the user explicitly asked to **update an existing Artifact** at that path. Do not treat a general request to publish as permission to replace one. `--dry-run` is useful for local validation, but it does not upload anything. Do not report a URL until the real publish succeeds. Normal success prints just the URL on stdout; warnings and errors go to stderr. `--json` instead prints metadata, not just the URL.

If publishing fails, use the CLI's `error: <code>: <message>` and exit status:

| Exit | Response |
| --- | --- |
| `2` | Usage or local validation error. For `auth_missing`, point the user to `pubhub login` (or `PUBHUB_TOKEN`); for other errors, fix the source/path or reported configuration issue locally. The CLI suggests a name but never rewrites it. |
| `3` | `exists`, `shape_conflict`, or `nesting_conflict`. Stop and ask the user whether to update, choose a different path, or resolve the conflict. Do not silently overwrite, rename, or delete. |
| `4` | Authentication or authorization (`401`/`403`). Point the user to `pubhub login` for a missing or expired PAT. For `403`, ask the owner to check the machine account's `publisher` grant in the Portal's configured Zitadel project and authorization organization. Do not print credentials. |
| `5` | `busy` or HTTP `503` after the CLI's three retries. Report the failure rather than claiming publication. |
| `1` | Other failure. Report the CLI error. |

## Describe the Project

After a successful publish, write a separate, one-sentence Project description. Use the same repository root chosen for the Project path, even if the Artifact came from a subdirectory:

1. Read the root README (`README`, `README.md`, or another `README.*`) and summarize what the Project does.
2. If it has no useful account of the Project, read other repository docs and summarize its purpose.
3. If there are no useful docs, inspect source code such as entry points, manifests and core modules. If its purpose remains unclear, ask the user rather than guess.

Keep the sentence factual, under 1,000 characters, and free of credentials or secrets. Then run `pubhub describe-project <project> "<one-sentence project description>"`. This command checks the Catalogue and skips Projects that already have a description. `-d` is the Artifact description and must stay about the Artifact. If the CLI lacks `describe-project`, or setting the Project description fails, say so; a successful Artifact publish is still a success, but the Project description was not set.

Return the printed URL and remind the user that it is **public but unlisted**.
