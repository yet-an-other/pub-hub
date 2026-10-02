# Machine Publishers use Zitadel PATs introspected by the Portal; one API behind two auth prefixes

Machine Publishers (agents, CLI) authenticate with Zitadel service-account PATs sent as `Authorization: Bearer`. The Portal introspects each token and admits an active account only when Zitadel returns its `publisher` role for the configured project and authorization organization. Token activity, audience, scopes, or an unqualified role name alone do not grant access: Zitadel can report PATs from elsewhere in the instance as active. We chose PATs over Zitadel's recommended private-key JWT profile because clients stay a static header and never need to reach the LAN-only Zitadel, and revocation is real; the price is that publishing depends on Zitadel being up. Because nginx `auth_request` cannot accept "cookie or bearer" on one path, the same Portal API is mounted twice: `/api/` for bearer tokens (no `auth_request`) and `/ui/api/` for the owner's SPA behind oauth2-proxy's cookie.

The original decision used a subject-to-label allowlist in Portal config. We replaced it with Zitadel role assignments and account names so adding, renaming, or removing a machine Publisher requires no Portal config edit or restart. The Portal still needs one-time global configuration identifying the trusted Zitadel project, authorization organization, and role. Browser sign-in and its owner-email checks remain separate. See [the source investigation](../research/zitadel-publisher-authorization.md); the deployed Zitadel version's PAT claims must be verified before migration.

## Considered Options

- **Portal-minted API keys**: no Zitadel dependency for machines, but a second credential system with its own mint/list/revoke UI.
- **Client credentials or private-key JWT → JWT access token**: offline validation (even by oauth2-proxy), but every client mints tokens at Zitadel and a leaked token lives until `exp`.
- **One prefix, routing on the `Authorization` header in nginx**: avoids the second prefix, at the cost of `if`-based routing in the auth-critical config.

## Consequences

- A Zitadel outage makes `/api/` answer `503` (not `401`); introspection results are cached for 60 s.
- Assign or remove the designated role in Zitadel to change machine access. Introspection supplies the account name for new publish records, falling back to username then subject ID; existing records retain their saved Publisher names.
- The Portal caches authorization and identity for at most 60 s. Role removal can take that long plus Zitadel's propagation delay; after cache expiry, an outage returns `503` rather than admitting a stale grant.
- Every authenticated Publisher has full rights; there are no per-credential scopes.
