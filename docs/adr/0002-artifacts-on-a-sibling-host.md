# Artifacts on a sibling host, not the Portal's host or a separate domain

Artifacts are served at the root of `pub.bdgn.me` and the Portal alone at `hub.bdgn.me`, overturning the original preference for one FQDN: on a shared origin, paths are no boundary and Artifact JS acts with the owner's Portal session, while `CSP: sandbox` contains it only by breaking storage and workers, with one missing header meaning full takeover (see [#2](https://github.com/yet-an-other/pub-hub/issues/2)). We stopped short of a separate registrable domain, which research recommended once it was known that another `bdgn.me` service (`kuber.bdgn.me`'s oauth2-proxy) sets a `Domain=bdgn.me` cookie, and accepted the same-site residual risk to avoid a second domain, zone and certificate.

## Considered Options

- **One FQDN, Artifacts under `/pub/` with `CSP: sandbox`**: rejected; breaks `localStorage`, IndexedDB, cookies and workers, and fails catastrophically on a single missed header.
- **Separate registrable domain for Artifacts**: removes all same-site exposure; declined as extra cost for a single-tenant hub whose Artifacts come from the owner and their agents.

## Consequences

- Residual risk, accepted: Artifact JS is same-site with every `*.bdgn.me` service, so it can toss `Domain=bdgn.me` cookies (including over the shared oauth2-proxy cookie; cookie-bomb DoS) and forge requests to any service relying on SameSite alone for CSRF. The Portal is covered by `__Host-` cookies and `http.CrossOriginProtection`.
- The Artifact hostname is configuration, so moving to a separate domain later changes URLs but not the design.
- Artifact bytes are never served on `hub.bdgn.me`; `hub.` has no redirect to `pub.`. Cloudflare Tunnel exposes `pub.bdgn.me` by hostname only.
