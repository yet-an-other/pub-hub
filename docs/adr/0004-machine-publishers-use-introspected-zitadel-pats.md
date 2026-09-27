# Machine Publishers use Zitadel PATs introspected by the Portal; one API behind two auth prefixes

Machine Publishers (agents, CLI) authenticate with Zitadel service-account PATs sent as `Authorization: Bearer`, which the Portal validates itself by introspection and admits only if the account's user id is on an allowlist in Portal config (Zitadel reports any PAT in the instance as active to the Portal). We chose PATs over Zitadel's recommended private-key JWT profile because clients stay a static header and never need to reach the LAN-only Zitadel, and revocation is real; the price is that publishing depends on Zitadel being up. Because nginx `auth_request` cannot accept "cookie or bearer" on one path, the same Portal API is mounted twice: `/api/` for bearer tokens (no `auth_request`) and `/ui/api/` for the owner's SPA behind oauth2-proxy's cookie.

## Considered Options

- **Portal-minted API keys**: no Zitadel dependency for machines, but a second credential system with its own mint/list/revoke UI.
- **Client credentials or private-key JWT → JWT access token**: offline validation (even by oauth2-proxy), but every client mints tokens at Zitadel and a leaked token lives until `exp`.
- **One prefix, routing on the `Authorization` header in nginx**: avoids the second prefix, at the cost of `if`-based routing in the auth-critical config.

## Consequences

- A Zitadel outage makes `/api/` answer `503` (not `401`); introspection results are cached for 60 s.
- Adding or removing a machine Publisher means editing the allowlist and restarting the Portal; the allowlist label is the Publisher identity shown in the Catalogue.
- Every authenticated Publisher has full rights; there are no per-credential scopes.
