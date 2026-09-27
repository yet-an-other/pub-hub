# Zitadel + oauth2-proxy + nginx: browser and machine auth

Research for [#3](https://github.com/yet-an-other/pub-hub/issues/3) (part of the map, #1).
Researched 2026-09-27 against primary sources only:

- **oauth2-proxy v7.15.4**: released docs (`7.15.x`) and source at commit
  [`4238ee1`](https://github.com/oauth2-proxy/oauth2-proxy/tree/4238ee1b32500b632032a24b898efe996f3b704b).
- **Zitadel**: official docs at zitadel.com/docs and source at commit
  [`5ca0b54`](https://github.com/zitadel/zitadel/tree/5ca0b54ca311c4be535589e7c375f9bea50e3ec2).
- **nginx**: official docs for `ngx_http_auth_request_module` and `ngx_http_proxy_module`.
- **systemd** and **Go** docs, for the systemd gotchas.

Links to source lines are pinned to those commits. Anything I could not confirm in a primary source is marked **(unverified)**.

---

## Recommendation (short)

1. **Put oauth2-proxy beside nginx with `auth_request`, not in front of the Portal as a reverse proxy.** nginx keeps routing: Artifact paths get no auth, and uploads never pass through oauth2-proxy. oauth2-proxy handles browser login only. nginx copies `X-Auth-Request-User`/`-Email` onto Portal requests. The Portal trusts those headers because it listens only on a unix socket that nginx alone can reach, and nginx always overwrites the headers.
2. **Configure oauth2-proxy as `provider = "oidc"` against Zitadel.** Use a Web app with code flow, `client_secret_basic` and PKCE S256. Restrict login three times: the Zitadel project setting *Check Role Assignment on Authentication*, oauth2-proxy `authenticated_emails_file` listing only the owner, and an owner check in the Portal. Use a `__Host-` cookie with `session_cookie_minimal` and an explicit `cookie_expire`.
3. **Machine Publishers use a bearer-only API prefix with no `auth_request`, and the Portal validates tokens itself.** oauth2-proxy cannot validate opaque tokens at all: it has no introspection, and it only accepts JWT-shaped bearers. It can validate Zitadel **JWT** access tokens, but that forces every client to mint tokens at the LAN-only token endpoint.
4. **Rank of machine credentials:** (1) **Zitadel service-account PAT + Portal introspection**; (2) client-credentials secret → JWT access token, validated offline by the Portal (or oauth2-proxy); (3) private-key JWT profile → same JWT, validated the same way. Details and trade-offs are in [§3.5](#35-ranking-for-pub-hub).

A full topology sketch is in [§6](#6-recommended-topology).

---

## 1. nginx `auth_request` vs oauth2-proxy as reverse proxy

### 1.1 How each mode works

**`auth_request` mode.** nginx sends a subrequest to oauth2-proxy's `/oauth2/auth` for every protected request.

- The endpoint "only returns a 202 Accepted response or a 401 Unauthorized response" ([endpoints](https://oauth2-proxy.github.io/oauth2-proxy/features/endpoints)). It returns 403 when the `allowed_groups` / `allowed_emails` / `allowed_email_domains` query-param constraints fail ([`AuthOnly`, oauthproxy.go#L1018-L1037](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/oauthproxy.go#L1018-L1037)).
- nginx treats a 2xx as allow, 401/403 as deny, and anything else as an error. The module "is not built by default"; it needs `--with-http_auth_request_module` ([nginx auth_request](https://nginx.org/en/docs/http/ngx_http_auth_request_module.html)). Check your build with `nginx -V`.
- oauth2-proxy's docs state: "This option requires `--reverse-proxy` option to be set" ([nginx integration](https://oauth2-proxy.github.io/oauth2-proxy/configuration/integrations/nginx)).
- Browser routes should turn a 401 into a **302** through a named location (`error_page 401 = @oauth2_signin; … return 302 /oauth2/sign_in?rd=…`). API routes should pass the 401 through. Since v7.14.2, `/oauth2/auth` always answers 401 (never 302), even with `skip_provider_button` ([CHANGELOG v7.14.2](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/CHANGELOG.md), [nginx integration: "Browser vs API Routes"](https://oauth2-proxy.github.io/oauth2-proxy/configuration/integrations/nginx)).
- The subrequest must not carry the body: `proxy_pass_request_body off; proxy_set_header Content-Length "";` (same nginx example). This matters for Bundle uploads.

**Reverse-proxy mode.** nginx → oauth2-proxy → Portal (`--upstream=…`).

- oauth2-proxy handles every Portal request itself and injects user headers upstream.
- Anonymous paths need `--skip-auth-route`. That flag was the precondition for the critical 2026 bypass [GHSA-7x63-xv5r-3p2x](https://github.com/oauth2-proxy/oauth2-proxy/security/advisories/GHSA-7x63-xv5r-3p2x) (spoofed `X-Forwarded-Uri`, fixed in v7.15.2 together with the new `--trusted-proxy-ip`).
- Upload bodies stream through oauth2-proxy. The default `--upstream-timeout` is `30s` ([config overview: Upstream Options](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview)).

### 1.2 Headers that reach the Portal

| Mode | Header | Source / flag | Value (Zitadel) |
|---|---|---|---|
| auth_request | `X-Auth-Request-User` | `set_xauthrequest` (response of `/oauth2/auth`) | session `user` = `sub` claim (default user claim is `sub`, [provider_data.go#L24](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/providers/provider_data.go#L24)) |
| auth_request | `X-Auth-Request-Email` | `set_xauthrequest` | `email` claim (userinfo) |
| auth_request | `X-Auth-Request-Preferred-Username` | `set_xauthrequest` | Zitadel login name `user@orgdomain` |
| auth_request | `X-Auth-Request-Groups` | `set_xauthrequest` | `groups` claim; Zitadel has none by default (see §2.3) |
| auth_request | `X-Auth-Request-Access-Token` | `set_xauthrequest` + `pass_access_token` | Zitadel access token |
| auth_request | `Authorization: Bearer <id_token>` | `set_authorization_header` | ID token |
| reverse proxy | `X-Forwarded-User`, `-Email`, `-Preferred-Username`, `-Groups` | `pass_user_headers` (default **true**) | as above |
| reverse proxy | `X-Forwarded-Access-Token` / `Authorization` | `pass_access_token` / `pass_authorization_header` | as above |

Header definitions: [legacy_options.go#L309 (`getPassUserHeaders`)](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/apis/options/legacy_options.go#L309) and [#L409 (`getXAuthRequestHeaders`)](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/apis/options/legacy_options.go#L409). Flag descriptions: [config overview: Header Options](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview).

In `auth_request` mode **nothing reaches the Portal automatically**. The `X-Auth-Request-*` headers are on the *subrequest response*. nginx must copy them explicitly:

```nginx
auth_request_set $pubhub_user  $upstream_http_x_auth_request_user;
auth_request_set $pubhub_email $upstream_http_x_auth_request_email;
proxy_set_header X-Auth-Request-User  $pubhub_user;
proxy_set_header X-Auth-Request-Email $pubhub_email;
```

The same applies to `X-Forwarded-For/-Proto/-Host`: nginx sends them only if configured. In reverse-proxy mode, `skip_auth_strip_headers` (default `true`) strips client-supplied copies of the headers oauth2-proxy would set ([config overview](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview)).

### 1.3 How the Portal can trust them

All of these are needed together. None is enough alone.

1. **Only nginx can connect to the Portal.** Bind the Portal to a unix socket (mode `0660`, group shared with nginx) rather than `127.0.0.1:port`, which any local process could reach.
2. **nginx always overwrites the identity headers.**
   - `proxy_set_header` replaces a client-sent header of the same name.
   - An empty value removes the header entirely: "If the value of a header field is an empty string then this field will not be passed" ([nginx proxy_set_header](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_set_header)).
   - On every location that proxies to the Portal *without* `auth_request` (the machine API, and Artifacts if the Portal serves them), set `proxy_set_header X-Auth-Request-User ""; proxy_set_header X-Auth-Request-Email "";`.
   - **Gotcha:** `proxy_set_header` directives "are inherited from the previous configuration level if and only if there are no `proxy_set_header` directives defined on the current level" (same page). Put the common set in an `include` snippet used by every location.
3. **The Portal reads identity headers only on browser routes** and compares them with the configured owner (`sub` and/or email). This is defence in depth if the oauth2-proxy allowlist is ever misconfigured. On machine routes it ignores these headers and cookies entirely.
4. **Prefer a configured base URL** (`https://hub.bdgn.me`) over trusting `X-Forwarded-Host/Proto` when the Portal builds absolute Artifact URLs. That removes another header the Portal would otherwise have to trust.
5. **Optional: verify identity cryptographically.** oauth2-proxy can hand nginx `Authorization: Bearer <id_token>` (`set_authorization_header`) for the Portal to verify against Zitadel's JWKS. Two catches:
   - It needs tokens in the cookie, which conflicts with `session_cookie_minimal`.
   - The ID token expires after 12 h (Zitadel default, [cmd/defaults.yaml#L1344-L1345](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/cmd/defaults.yaml#L1344-L1345)) while the oauth2-proxy session lasts until `cookie_expire`. Keeping the forwarded token fresh would also need `cookie_refresh` plus `offline_access`.

   Not worth it for a single-owner LAN deployment.

### 1.4 Choice

`auth_request` wins for pub-hub:

- nginx already fronts S3 and is where Artifact routing lives.
- Anonymous Artifact paths are simply locations without `auth_request`, with no `skip_auth_routes` regexes.
- Bundle uploads go straight to the Portal.
- The machine API can bypass oauth2-proxy cleanly.

Reverse-proxy mode would only help if nginx were absent.

---

## 2. oauth2-proxy + Zitadel OIDC essentials

### 2.1 Zitadel side

oauth2-proxy has no Zitadel-specific provider. Use the generic `oidc` provider. Zitadel's own guide ([OAuth 2.0 Proxy example](https://zitadel.com/docs/examples/identity-proxy/oauth2-proxy)) uses a **Web** application, **Authorization Code**, auth method **BASIC**.

- **Project** `pub-hub` with roles, e.g. `owner` and `publisher`.
  - Enable **Check Role Assignment on Authentication**: "Users are **only** allowed to log in if they have at least one role assigned for this project" ([projects overview](https://zitadel.com/docs/guides/manage/console/projects-overview)).
  - The check runs in the interactive auth-request flow ([auth_request.go#L1839](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/internal/auth/repository/eventsourcing/eventstore/auth_request.go#L1839)). It does **not** gate service-account token grants: client credentials and JWT profile never pass through it.
- **App `hub-browser`** (Web, code flow, `client_secret_basic`; PKCE can be added from the oauth2-proxy side).
  - Redirect URI `https://hub.bdgn.me/oauth2/callback`; post-logout URI `https://hub.bdgn.me/`.
  - Refresh-token grant is needed only if you use `cookie_refresh`.
- **App `hub-api`** (type API, Basic or Private Key JWT). These are the Portal's credentials for **introspection** (§3.3).
- **User grant**: the owner's user gets role `owner`.
- **Endpoints** ([endpoints reference](https://zitadel.com/docs/apis/openidoauth/endpoints)): `/.well-known/openid-configuration`, `/oauth/v2/authorize`, `/oauth/v2/token`, `/oauth/v2/introspect`, `/oidc/v1/userinfo`, `/oidc/v1/end_session`, `/oauth/v2/keys` (JWKS; "keys can be rotated without prior notice").

### 2.2 oauth2-proxy config essentials

```toml
# /etc/oauth2-proxy/oauth2-proxy.cfg   (oauth2-proxy >= v7.15.4)
http_address          = "127.0.0.1:4180"             # default; or "unix:///run/oauth2-proxy/o2p.sock,mode=0660"
reverse_proxy         = true                         # required for auth_request
trusted_proxy_ips     = ["127.0.0.1/32", "::1/128"]  # only nginx may send X-Forwarded-*/X-Real-IP
real_client_ip_header = "X-Real-IP"

provider              = "oidc"
provider_display_name = "Zitadel"
oidc_issuer_url       = "https://<zitadel-host>"     # must equal Zitadel's `issuer` byte-for-byte
client_id             = "<hub-browser client id>"
redirect_url          = "https://hub.bdgn.me/oauth2/callback"
scope                 = "openid email profile"       # = oidc provider default; add offline_access only with cookie_refresh
code_challenge_method = "S256"
skip_provider_button  = true

authenticated_emails_file = "/etc/oauth2-proxy/allowed-emails"   # one line: the owner's email
# NOT: email_domains = ["*"];  NOT: user_id_claim = "sub"  (see §2.3)

set_xauthrequest       = true
session_cookie_minimal = true                        # no tokens in the cookie -> single small cookie
cookie_name            = "__Host-pubhub"
cookie_secure          = true                        # default
cookie_httponly        = true                        # default
cookie_samesite        = "lax"
cookie_csrf_samesite   = "lax"                       # v7.15.0+
cookie_expire          = "12h"                       # never 0 (see below)
whitelist_domains      = ["hub.bdgn.me", "<zitadel-host>"]
# client_secret_file / cookie_secret_file: passed on the command line from systemd credentials (§5)
```

The flag semantics come from the [config overview](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview). Points that are not obvious:

- **`oidc_issuer_url`** triggers OIDC discovery **at startup**, and a failure aborts start ([provider_verifier.go#L153-L156](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/providers/oidc/provider_verifier.go#L153-L156)). See §5 for the systemd consequence.
  - Zitadel picks the instance, and therefore the issuer, from the **Host header**; "one instance normally runs on one domain and represents one issuer" ([custom domain](https://zitadel.com/docs/self-hosting/manage/custom-domain), [instance](https://zitadel.com/docs/concepts/structure/instance)).
  - So every party must use the same canonical Zitadel hostname.
- **Email claim.**
  - Zitadel puts `email` in the ID token only for `response_type=id_token` (claims matrix in [claims](https://zitadel.com/docs/apis/openidoauth/claims)). In code flow, oauth2-proxy fetches missing claims from the discovered userinfo endpoint using the access token ([claim_extractor.go#L88](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/providers/util/claim_extractor.go#L88)), so it works without extra settings.
  - An `email_verified=false` claim makes the login fail unless `insecure_oidc_allow_unverified_email` is set ([provider_data.go#L294-L308](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/providers/provider_data.go#L294-L308)).
- **`whitelist_domains`** is needed twice:
  - The nginx example's absolute `rd=$scheme://$host$request_uri` is rejected unless its host is whitelisted. Only relative paths pass without it ([redirect validator](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/app/redirect/validator.go)).
  - Sign-out redirects to Zitadel's `end_session` ([endpoints: Sign out](https://oauth2-proxy.github.io/oauth2-proxy/features/endpoints)).
- **Sign-out URL:** `/oauth2/sign_out?rd=<urlencoded https://<zitadel-host>/oidc/v1/end_session?client_id=<id>&post_logout_redirect_uri=https://hub.bdgn.me/>`.
  - Zitadel validates `post_logout_redirect_uri` against `client_id` when there is no `id_token_hint` ([end_session_endpoint](https://zitadel.com/docs/apis/openidoauth/endpoints)).
  - This matters because `session_cookie_minimal` keeps no ID token for the `{id_token}` placeholder.
- **Session lifetime.**
  - With `cookie_refresh` unset, oauth2-proxy never re-validates a stored session ([`needsRefresh`, stored_session.go#L244](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/middleware/stored_session.go#L244)). A session is valid until the **signed cookie timestamp** is older than `cookie_expire` ([encryption/utils.go#L45-L61](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/encryption/utils.go#L45)).
  - The default is `168h`. `cookie_expire = 0` disables the server-side age check entirely.
  - Set an explicit value, e.g. `12h`, to match Zitadel's token lifetime.
  - `cookie_refresh` is incompatible with `session_cookie_minimal` ([validation/sessions.go#L35-L38](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/validation/sessions.go#L35-L38)).
- **Cookie secret:** `cookie_secret_file` "must be raw binary, exactly 16, 24, or 32 bytes" (`head -c32 /dev/urandom`), with no trailing newline.

### 2.3 Restricting login to the owner

Three layers, cheapest first:

1. **Zitadel:** *Check Role Assignment on Authentication* on the project, and grant a role only to the owner (§2.1). Other users of the LAN Zitadel instance cannot even complete login to `hub-browser`.
2. **oauth2-proxy:** `authenticated_emails_file` with only the owner's address.
   - The file is watched and hot-reloaded ([validator.go#L24-L36](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/validator.go#L24-L36)).
   - Config validation requires `email_domains` **or** `authenticated_emails_file`, so the file alone is fine ([validation/options.go#L50-L53](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/validation/options.go#L50-L53)).
   - The callback admits a session only if `Validator(session.Email) && authorized` ([oauthproxy.go#L964](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/oauthproxy.go#L964)).
3. **Portal:** compare `X-Auth-Request-Email` (or `-User` = Zitadel user id) with its configured owner.

Traps in Zitadel's own oauth2-proxy example, which sets `email_domains = ["*"]` and `user_id_claim = "sub"` ([Zitadel guide](https://zitadel.com/docs/examples/identity-proxy/oauth2-proxy)):

- `email_domains=["*"]` admits **every** user of the Zitadel instance.
- `user_id_claim="sub"` silently re-points the *email* claim to `sub` through a backwards-compat shim ([providers.go#L160-L169](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/providers/providers.go#L160-L169); open bug [oauth2-proxy#3439](https://github.com/oauth2-proxy/oauth2-proxy/issues/3439)). `X-Auth-Request-Email` would then carry the numeric user id.
- `allowed_groups` does not work with Zitadel roles out of the box. The roles claim `urn:zitadel:iam:org:project:roles` is a JSON **object** (`{"role": {"orgId": "domain"}}`, [claims](https://zitadel.com/docs/apis/openidoauth/claims)). oauth2-proxy coerces a non-array claim into a single string ([util.go#L225](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/util/util.go#L225)). Zitadel's guide therefore needs an Action to flatten roles into a list; not worth it for one owner.

### 2.4 Cookie scope and the same-origin Artifacts question (#2)

- `__Host-` requires `Secure`, `Path=/`, and no `Domain`. That matches the oauth2-proxy defaults (`cookie_path="/"`, no `cookie_domains`), and the docs recommend the prefix ([config overview: Cookie Options](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview)).
- **Consequence:** the session cookie is sent with *every* request to `hub.bdgn.me`, including Artifact requests and same-origin `fetch()` from Artifact JS. It is `HttpOnly`, so JS cannot read it, but it can still *use* it.
- **oauth2-proxy provides no CSRF or Origin protection for upstream requests.** Its CSRF cookie only protects the OAuth `state` round trip. The issue tracker documents that SameSite does not stop same-site, cross-origin requests ([#2081](https://github.com/oauth2-proxy/oauth2-proxy/issues/2081), [#2573](https://github.com/oauth2-proxy/oauth2-proxy/issues/2573), open).
  - So any cookie-authenticated Portal route must do its own `Origin` / `Sec-Fetch-Site` checks, and same-origin Artifacts need containment. That is #2's scope.
  - A sibling host `pub.bdgn.me` is **same-site** with `hub.bdgn.me`, so SameSite does not separate them either.
  - Same-origin Artifact JS can also call `/oauth2/userinfo` (it returns the owner's email) and `/oauth2/sign_out`.
- **Bearer-only machine routes are immune** to ambient-cookie abuse, which is one more reason to keep them cookie-free.
- **SameSite:** use `lax`.
  - `strict` breaks the callback's CSRF-cookie check when the IdP is cross-site ([#1663](https://github.com/oauth2-proxy/oauth2-proxy/issues/1663)). v7.15.0 added `cookie_csrf_samesite` to set that cookie separately ([CHANGELOG v7.15.0](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/CHANGELOG.md)).
  - `strict` for the session cookie is only safe if Zitadel shares the site (`*.bdgn.me`). **(unverified in a browser)**
- **If #2/#5 give the Portal a reserved prefix** (e.g. `/_/`), there is an alternative to `__Host-`:
  - Use `cookie_name="__Secure-pubhub"`, `cookie_path="/_/"`, `proxy_prefix="/_/oauth2"`, and redirect URI `https://hub.bdgn.me/_/oauth2/callback`.
  - This keeps the cookie off Artifact requests entirely: it is not sent to `/pub/…`, and not through a future Cloudflare Tunnel.
  - It is **not** a security boundary against same-origin JS (#2), but it narrows exposure.
  - The `auth_request` subrequest carries the original request's cookies, so the cookie path must cover every Portal path.

---

## 3. Machine credentials

### 3.1 What Zitadel offers for service accounts (formerly "service/machine users")

Source: [authenticate service accounts](https://zitadel.com/docs/guides/integrate/service-accounts/authenticate-service-accounts).

| Credential | How the client gets a bearer | Token on the wire | Lifetime |
|---|---|---|---|
| **PAT** | Created in Console or API; used directly as `Authorization: Bearer <PAT>` | **Opaque**. The value is `EncryptToken(tokenID + ":" + userID)` ([user_personal_access_token.go#L116](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/internal/command/user_personal_access_token.go#L116)), not a JWT | Optional expiry date, or none ([PAT guide](https://zitadel.com/docs/guides/integrate/service-accounts/personal-access-token)) |
| **Client credentials** | `POST /oauth/v2/token` with `grant_type=client_credentials`, Basic auth with the service account's client id/secret | **Opaque by default**, JWT if the service account's *Access Token Type* is JWT ([client credentials guide](https://zitadel.com/docs/guides/integrate/service-accounts/client-credentials)) | Access token 12 h by default ([defaults.yaml#L1344](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/cmd/defaults.yaml#L1344)) |
| **Private-key JWT (JWT profile, RFC 7523)** | Client signs an RS256 assertion (`iss`=`sub`=userId, `aud`=Zitadel domain, `iat` ≤ 1 h old) with a downloaded key, then `POST /oauth/v2/token` with `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer` ([private key JWT guide](https://zitadel.com/docs/guides/integrate/service-accounts/private-key-jwt)) | Same as above (opaque or JWT by *Access Token Type*) | 12 h default |

Zitadel's recommendation is private-key JWT. It warns that PATs "are long-lived tokens … if leaked the attacker can access all resources until the PAT is expired or deleted" (same page).

**Audience matters.** For client credentials and JWT profile, the access token's `aud` starts **empty**. It only gains project IDs requested through the scope `urn:zitadel:iam:org:project:id:<projectId>:aud` ([token_client_credentials.go#L40](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/internal/api/oidc/token_client_credentials.go#L40), [token_jwt_profile.go#L51](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/internal/api/oidc/token_jwt_profile.go#L51), [domain/token.go#L10](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/internal/domain/token.go#L10)). The docs say: without that scope "token introspection will fail." Machine clients must request `scope=openid urn:zitadel:iam:org:project:id:<pub-hub project id>:aud`.

**Claims in a Zitadel JWT access token**: `iss`, `sub`, `aud`, `azp`/`client_id`, `exp`, `iat`, `nbf`, `jti`, plus roles/metadata when requested. There is **no `email`** and **no `preferred_username`** ([claims matrix](https://zitadel.com/docs/apis/openidoauth/claims)).

### 3.2 Can oauth2-proxy validate them?

**JWT access tokens: yes, with `skip_jwt_bearer_tokens = true`.**

- The bearer must match `^ey…\.ey…\.…$` ([jwt_session.go#L16](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/middleware/jwt_session.go#L16)). It is verified by the provider's verifier through `CreateSessionFromToken` ([oidc.go#L211](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/providers/oidc.go#L211)): issuer, signature (discovered JWKS), expiry, and `aud` ∈ {`client_id`} ∪ `oidc_extra_audiences` ([verifier.go#L63](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/providers/oidc/verifier.go#L63)).
- So set **`oidc_extra_audiences = ["<pub-hub project id>"]`**.
- `extra_jwt_issuers` (`issuer=audience` pairs, [validation/options.go#L55-L72](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/validation/options.go#L55-L72)) is only needed for a *different* issuer. It is not needed for Zitadel-as-provider.
- Because the token has no email, oauth2-proxy sets `Email = User = sub` ([oidc.go#L223-L226](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/providers/oidc.go#L223-L226)), and the email allowlist is then applied to that `sub` ([oauthproxy.go#L1154](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/oauthproxy.go#L1154)). To admit a service account, **add its numeric user id as a line in `authenticated_emails_file`**. The file is a plain string set. The alternative, `email_domains=["*"]`, opens browser login to everyone.
- `X-Auth-Request-User`/`-Email` then both carry the service account's `sub`.
- Set `bearer_token_login_fallback = false` so invalid JWTs get **403** instead of being treated as anonymous ([jwt_session.go](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/middleware/jwt_session.go); [behaviour](https://oauth2-proxy.github.io/oauth2-proxy/behaviour)).
- Validation is **offline**: a revoked token stays valid until `exp`.

**Opaque tokens (PAT, or default opaque client-credential tokens): no.**

- The source contains no introspection code at all. The RFC 7662 feature request [#3058](https://github.com/oauth2-proxy/oauth2-proxy/issues/3058) was closed "not planned" (2025-11).
- A non-JWT bearer fails `findTokenFromHeader` ([jwt_session.go#L96](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/middleware/jwt_session.go#L96)). The result is 403 with `bearer_token_login_fallback=false`, otherwise "no session", which means 401 from `/oauth2/auth`.
- So a PAT **must** be validated by the Portal, on a route that bypasses `auth_request`.

### 3.3 Portal-side validation paths

**Introspection** (`POST /oauth/v2/introspect`, [token introspection](https://zitadel.com/docs/guides/integrate/token-introspection), [basic auth](https://zitadel.com/docs/guides/integrate/token-introspection/basic-auth)):

- Works for **opaque and JWT** tokens and reflects revocation.
- The Portal authenticates as the `hub-api` API app (Basic or private_key_jwt). The response is `{active, sub, username, aud, client_id, exp, scope, …}`; `active:false` means the token is invalid or not meant for this client.
- **PAT subtlety (source-verified):** when a PAT is introspected, Zitadel *appends the introspecting client's own client id and project id to the token's audience* ([access_token.go#L118-L132](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/internal/api/oidc/access_token.go#L118-L132), [introspect.go#L90-L97](https://github.com/zitadel/zitadel/blob/5ca0b54ca311c4be535589e7c375f9bea50e3ec2/internal/api/oidc/introspect.go#L90-L97)).
  - So **any** valid PAT of **any** service account in the instance introspects as `active:true` for the Portal.
  - The Portal **must authorize by `sub`**, using an allowlist of service-account user ids, or by a role grant.
  - For PATs, introspection also adds the project's role scopes, so `urn:zitadel:iam:org:project:roles` should appear for users with grants. **(unverified end-to-end; fall back to the `sub` allowlist)**
- Go: `github.com/zitadel/oidc/v3/pkg/client/rs` offers `NewResourceServerClientCredentials(ctx, issuer, clientID, secret)` and `rs.Introspect[*oidc.IntrospectionResponse](ctx, rs, token)` ([resource_server.go](https://github.com/zitadel/oidc/blob/main/pkg/client/rs/resource_server.go)).
- Cache results briefly (e.g. 60 s, keyed by SHA-256 of the token) so a Bundle upload does not introspect on each retry.

**JWKS** (JWT only): verify offline against `/oauth/v2/keys`.

- Use `github.com/coreos/go-oidc/v3` (the library oauth2-proxy uses), with `SkipClientIDCheck`. Then check that `aud` contains the project id and `sub` is allowlisted.
- Keys rotate without notice, so use a key set that refetches on unknown `kid` ([jwks_uri](https://zitadel.com/docs/apis/openidoauth/endpoints)).
- Not revocation-aware.

### 3.4 Option matrix

| | PAT (opaque) | Client credentials → JWT | Private-key JWT → JWT |
|---|---|---|---|
| Stored on agent host | PAT (bearer itself) | client id + secret | private key (JSON) |
| Client work | none; static header | 1 POST per ~12 h, cache token | sign RS256 assertion + POST |
| Agent must reach Zitadel | **no** | yes (token endpoint) | yes (token endpoint) |
| oauth2-proxy can validate | **no** | yes (`skip_jwt_bearer_tokens` + `oidc_extra_audiences` + sub in emails file) | yes (same) |
| Portal validation | introspection (LAN call) | JWKS offline, or introspection | JWKS offline, or introspection |
| Revocation | delete PAT or deactivate user → next introspection (after cache TTL) | only at `exp` with JWKS; immediate with introspection | same |
| Leak impact | usable until expiry/deletion, from anywhere that reaches the Portal API | secret mints tokens only where Zitadel is reachable (LAN/VPN today) | key never sent over the wire; mints only where Zitadel is reachable |

### 3.5 Ranking for pub-hub

1. **PAT + Portal introspection.** Recommended.
   - Minimal client: a CLI or agent skill just sends `Authorization: Bearer $PUBHUB_TOKEN`.
   - Clients never talk to Zitadel, so this keeps working for remote agents if the publish API is ever opened while Zitadel stays LAN-only.
   - Revocation is real. The Portal already needs *some* Zitadel-aware code, and introspection covers every token type.
   - Mitigations: one service account and PAT per agent host; set an expiry; allowlist `sub` values; short introspection cache.
2. **Client-credentials secret → JWT access token.**
   - Choose this if you want zero Portal token code (oauth2-proxy validates), or offline validation.
   - Costs: token minting in every client; Zitadel reachability from every client. It also forces a single-issuer JWT path through oauth2-proxy, with the `sub`-in-emails-file workaround.
3. **Private-key JWT profile.** The strongest credential (Zitadel's recommendation), with the same token as (2) but the most client complexity. It is overkill while clients are the owner's own machines on the LAN.

Non-options:

- **Opaque tokens through oauth2-proxy**: impossible.
- **`email_domains=["*"]` so JWTs pass**: admits every Zitadel identity.
- **Relying on project *Check Role Assignment* to gate machines**: it does not apply to token grants (§2.1).

---

## 4. If Zitadel stays LAN-only while Artifacts become public

Who must reach Zitadel, and for what:

| Party | Needs Zitadel for | Effect of public Artifacts + LAN-only Zitadel |
|---|---|---|
| Reader | nothing | none. Artifact paths never touch oauth2-proxy or Zitadel |
| Owner's browser | `/oauth/v2/authorize`, login UI, `/oidc/v1/end_session` (browser redirects) | unchanged on LAN/VPN; login impossible off-LAN (by design today) |
| oauth2-proxy | discovery at **startup**, token + userinfo at login, JWKS | server-side over LAN; unaffected |
| Portal | introspection (PAT) or JWKS | server-side over LAN; unaffected |
| CLI / agent | PAT: nothing; JWT options: `/oauth/v2/token` | PAT works anywhere the publish API is reachable; JWT options need Zitadel reachability |

Things to get right:

- **Redirect leakage.**
  - Only Artifact paths may be exposed (tunnel ingress rules). `/oauth2/*` and Portal paths must not be routed publicly.
  - In nginx, the Artifact location must answer 404 itself for unknown paths and **never fall through** into the `auth_request`-protected `location /`.
  - Otherwise a public Reader hitting a wrong path gets a 302 to `/oauth2/sign_in` and then to the LAN-only Zitadel host. That is a dead end and discloses the internal hostname.
- **One issuer hostname.** If Zitadel is ever reachable under two names (LAN name vs tunnel name), tokens carry different `iss`. oauth2-proxy and the Portal accept exactly one issuer. Use split-horizon DNS for a single canonical Zitadel name.
- **Cookies through the tunnel.**
  - With the same FQDN exposed publicly, the owner's browser sends the `Path=/` session cookie with Artifact requests that go through Cloudflare, which terminates TLS.
  - A reserved Portal prefix with a path-scoped `__Secure-` cookie (§2.4) avoids that.
- **Opening Portal login publicly later** requires exposing Zitadel's browser-facing endpoints under the same issuer name, or keeping VPN. Opening **machine publishing** publicly works with PATs without exposing Zitadel.

---

## 5. Running oauth2-proxy without Docker (binary + systemd)

- **Install.**
  - Download the release tarball from [GitHub releases](https://github.com/oauth2-proxy/oauth2-proxy/releases) and verify with `sha256sum -c sha256sum.txt` ([installation](https://oauth2-proxy.github.io/oauth2-proxy/installation)). It is a single static Go binary.
  - Use **≥ v7.15.2**; current is v7.15.4. v7.15.2 fixed several critical auth bypasses ([CHANGELOG](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/CHANGELOG.md)).
  - Without a package manager, updates are manual: watch releases and advisories.
- **Unit file.** Start from [`contrib/oauth2-proxy.service.example`](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/contrib/oauth2-proxy.service.example). It has a dedicated user, `Restart=on-failure`, `After=/Wants=network-online.target`, and hardening (`ProtectSystem=full`, `NoNewPrivileges`, …).
- **Gotcha: drop its `ExecReload=/bin/kill -HUP $MAINPID`.**
  - oauth2-proxy only traps SIGINT/SIGTERM ([oauthproxy.go#L278](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/oauthproxy.go#L278)).
  - A Go program exits on an unhandled SIGHUP ([os/signal](https://pkg.go.dev/os/signal)).
  - systemd counts death by SIGHUP as a *clean* exit, which `Restart=on-failure` does **not** restart (`systemd.service(5)`, `Restart=`).
  - So `systemctl reload` would silently stop the service. Config changes need `restart`. `authenticated_emails_file` is watched and re-read (§2.3). `client_secret_file` is re-read when rotated ([nginx integration](https://oauth2-proxy.github.io/oauth2-proxy/configuration/integrations/nginx), note at the end).
- **Gotcha: startup needs Zitadel.** OIDC discovery runs at start and aborts on failure (§2.2).
  - Keep `After=network-online.target`, add `After=zitadel.service` if co-located, and rely on `Restart=on-failure` with `RestartSec`.
  - The server must resolve the canonical Zitadel hostname (split DNS or `/etc/hosts`).
  - If Zitadel uses a private CA, set `provider_ca_files` (plus `use_system_trust_store = true` to keep public roots).
- **Secrets.** Keep them out of the TOML with systemd credentials and the `%d` specifier ($CREDENTIALS_DIRECTORY, `systemd.unit(5)`):
  ```ini
  LoadCredential=client-secret:/etc/oauth2-proxy/client-secret
  LoadCredential=cookie-secret:/etc/oauth2-proxy/cookie-secret
  ExecStart=/usr/local/bin/oauth2-proxy --config=/etc/oauth2-proxy/oauth2-proxy.cfg \
      --client-secret-file=%d/client-secret --cookie-secret-file=%d/cookie-secret
  ```
  Both files must have no trailing newline; the cookie secret is raw 16/24/32 bytes (§2.2).
  - Config files take no env-var expansion. Env vars (`OAUTH2_PROXY_*`) also work but leak into `/proc/<pid>/environ`.
- **Listener.**
  - The default is `127.0.0.1:4180`. With a TCP listener, set `trusted_proxy_ips` to loopback. It otherwise defaults to *all IPs* for backwards compatibility and logs a warning ([oauthproxy.go#L401-L408](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/oauthproxy.go#L401-L408)).
  - Alternatively, listen on `unix:///run/oauth2-proxy/o2p.sock,mode=0660` with `RuntimeDirectory=oauth2-proxy`. Unix-socket peers are always trusted for forwarded headers ([scope.go#L71-L78](https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/pkg/apis/middleware/scope.go#L71-L78)), so file permissions are the boundary. The socket also works with systemd socket activation (`--http-address=fd:3`, [systemd socket](https://oauth2-proxy.github.io/oauth2-proxy/configuration/systemd_socket)).
- **Validation.** `oauth2-proxy --config … --config-test` exits 0/1 and can serve as `ExecStartPre=` ([config overview](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview)).
- **Do not set `ping_user_agent`.** It was the vector of critical [GHSA-5hvv-m4w4-gf6v](https://github.com/oauth2-proxy/oauth2-proxy/security/advisories/GHSA-5hvv-m4w4-gf6v).
- **Clock sync.** Needed for JWT `exp`/`iat` and Zitadel's ≤1 h assertion `iat`.

---

## 6. Recommended topology

```
                        hub.bdgn.me (nginx, TLS)
 Reader ───────────────▶ /pub/…                → Artifacts (anonymous; no auth_request; identity headers cleared)
 Owner browser ────────▶ /oauth2/…             → oauth2-proxy (127.0.0.1:4180 or unix socket)
                         = /oauth2/auth (internal) ◀─ auth_request subrequests
                         / (SPA + browser API) → auth_request → Portal (unix socket) + X-Auth-Request-User/-Email
 Agent / CLI ──────────▶ /api/… (bearer only)  → Portal (unix socket); no auth_request; Portal validates bearer
                                                   │
 oauth2-proxy ──LAN──▶ Zitadel: discovery, code exchange, userinfo, JWKS
 Portal ───────LAN──▶ Zitadel: /oauth/v2/introspect (hub-api credentials)
```

(`/pub/` and `/api/` are placeholders; #5 owns the URL layout.)

nginx sketch, following the official example ([nginx integration](https://oauth2-proxy.github.io/oauth2-proxy/configuration/integrations/nginx)) with the GHSA-7x63 mitigation (`internal`, forced `X-Forwarded-Uri`):

```nginx
# snippets/pubhub-proxy.conf: include in EVERY location that proxies to the Portal,
# because a location's own proxy_set_header lines disable inheritance from server level.
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-Proto $scheme;
# Identity headers are NOT in the snippet: each location sets them exactly once (value or "").

location /oauth2/ {
    proxy_pass http://127.0.0.1:4180;
    proxy_set_header Host                    $host;
    proxy_set_header X-Real-IP               $remote_addr;
    proxy_set_header X-Auth-Request-Redirect $request_uri;
}
location = /oauth2/auth {
    internal;
    proxy_pass http://127.0.0.1:4180;
    proxy_set_header Host            $host;
    proxy_set_header X-Real-IP       $remote_addr;
    proxy_set_header X-Forwarded-Uri $request_uri;
    proxy_set_header Content-Length  "";
    proxy_pass_request_body off;
}

location / {                                   # Portal SPA + cookie-authenticated browser API
    auth_request /oauth2/auth;
    error_page 401 = @oauth2_signin;           # browser: 401 -> 302 to login; the SPA should reload on a failed XHR
    auth_request_set $pubhub_user  $upstream_http_x_auth_request_user;
    auth_request_set $pubhub_email $upstream_http_x_auth_request_email;
    include snippets/pubhub-proxy.conf;
    proxy_set_header X-Auth-Request-User  $pubhub_user;
    proxy_set_header X-Auth-Request-Email $pubhub_email;
    proxy_pass http://unix:/run/pubhub/portal.sock;
}
location @oauth2_signin {
    return 302 /oauth2/sign_in?rd=$scheme://$host$request_uri;   # host must be in whitelist_domains
}

location /api/ {                               # machine Publishers: bearer only
    include snippets/pubhub-proxy.conf;
    proxy_set_header X-Auth-Request-User  "";  # empty value = header removed, client copy dropped
    proxy_set_header X-Auth-Request-Email "";
    client_max_body_size 200m;
    proxy_pass http://unix:/run/pubhub/portal.sock;
}
```

Before going live, run `nginx -T` and send a request with a forged `X-Auth-Request-Email` to a header-echo upstream. Confirm the forged value never arrives on either location. This is cheap insurance against config drift.

Portal responsibilities:

- On browser routes, require `X-Auth-Request-Email`/`-User` equal to the configured owner, plus Origin/`Sec-Fetch-Site` checks on state-changing requests (#2).
- On `/api/`, ignore cookies and identity headers. Require `Authorization: Bearer`, introspect with `hub-api`, and require `active && sub ∈ allowlist` (or the `publisher` role).

---

## 7. Surfaced questions (for the map)

- **Browser API vs machine API split.**
  - nginx `auth_request` takes a fixed URI, so a single API path cannot be cookie-auth *or* bearer-auth without extra nginx trickery.
  - Either the SPA and machine clients use separate prefixes (this doc's assumption), or the SPA's uploads go through a separate cookie route.
  - This constrains #5 (URL layout) and #8 (Publish API contract).
- **Reserved Portal prefix + path-scoped cookie** (§2.4): adopt `proxy_prefix=/<portal>/oauth2` so the session cookie never accompanies Artifact requests or crosses Cloudflare. This belongs to #2/#5.
- **Who issues machine credentials:** Zitadel PATs (this doc) or Portal-minted API keys, which would drop the introspection dependency but duplicate credential management. Worth a line in the auth-topology decision (#7).
- **Portal authorization source:** configured `sub` allowlist vs Zitadel role grants. The latter needs a quick end-to-end check that PAT introspection returns roles.
- **Session revocation:** with `session_cookie_minimal` and no refresh, disabling the owner in Zitadel does not end an existing Portal session until `cookie_expire`. Is 12 h acceptable, or is `cookie_refresh` + `offline_access` wanted (which gives up the minimal cookie)?

---

## Sources

oauth2-proxy (docs, release 7.15.x):
- Configuration overview (all flags): https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview
- nginx integration: https://oauth2-proxy.github.io/oauth2-proxy/configuration/integrations/nginx
- Endpoints: https://oauth2-proxy.github.io/oauth2-proxy/features/endpoints
- Behaviour: https://oauth2-proxy.github.io/oauth2-proxy/behaviour
- systemd socket: https://oauth2-proxy.github.io/oauth2-proxy/configuration/systemd_socket
- Installation: https://oauth2-proxy.github.io/oauth2-proxy/installation

oauth2-proxy (source, commit `4238ee1`, advisories, issues):
- Source tree: https://github.com/oauth2-proxy/oauth2-proxy/tree/4238ee1b32500b632032a24b898efe996f3b704b (files linked inline)
- CHANGELOG: https://github.com/oauth2-proxy/oauth2-proxy/blob/4238ee1b32500b632032a24b898efe996f3b704b/CHANGELOG.md
- GHSA-7x63-xv5r-3p2x: https://github.com/oauth2-proxy/oauth2-proxy/security/advisories/GHSA-7x63-xv5r-3p2x
- GHSA-5hvv-m4w4-gf6v: https://github.com/oauth2-proxy/oauth2-proxy/security/advisories/GHSA-5hvv-m4w4-gf6v
- Issues: #1663, #2081, #2573, #3058, #3439 (https://github.com/oauth2-proxy/oauth2-proxy/issues/<n>)

Zitadel (docs):
- oauth2-proxy example: https://zitadel.com/docs/examples/identity-proxy/oauth2-proxy
- Authenticate service accounts: https://zitadel.com/docs/guides/integrate/service-accounts/authenticate-service-accounts
- PAT: https://zitadel.com/docs/guides/integrate/service-accounts/personal-access-token
- Client credentials: https://zitadel.com/docs/guides/integrate/service-accounts/client-credentials
- Private key JWT: https://zitadel.com/docs/guides/integrate/service-accounts/private-key-jwt
- Token introspection: https://zitadel.com/docs/guides/integrate/token-introspection and https://zitadel.com/docs/guides/integrate/token-introspection/basic-auth
- Claims: https://zitadel.com/docs/apis/openidoauth/claims
- Scopes: https://zitadel.com/docs/apis/openidoauth/scopes
- Endpoints: https://zitadel.com/docs/apis/openidoauth/endpoints
- Projects (role check): https://zitadel.com/docs/guides/manage/console/projects-overview
- External access / custom domain: https://zitadel.com/docs/self-hosting/manage/custom-domain
- Instance concept: https://zitadel.com/docs/concepts/structure/instance

Zitadel (source, commit `5ca0b54`), linked inline:
- PAT creation and default scopes: `internal/command/user_personal_access_token.go`, `internal/api/grpc/user/v2/pat.go`
- Access-token verification and PAT audience extension: `internal/api/oidc/access_token.go`, `internal/api/oidc/introspect.go`
- Token audience from `:aud` scopes: `internal/domain/token.go`, `internal/api/oidc/token_client_credentials.go`, `internal/api/oidc/token_jwt_profile.go`
- JWT vs opaque issuance: `internal/api/oidc/token.go#L43`
- Project role check: `internal/auth/repository/eventsourcing/eventstore/auth_request.go#L1839`
- Defaults (token lifetimes): `cmd/defaults.yaml`

zitadel/oidc (Go resource-server client): https://github.com/zitadel/oidc/blob/main/pkg/client/rs/resource_server.go

nginx:
- auth_request module: https://nginx.org/en/docs/http/ngx_http_auth_request_module.html
- proxy_set_header: https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_set_header

systemd / Go:
- `systemd.service(5)` `Restart=` (clean-exit signals SIGHUP/SIGINT/SIGTERM/SIGPIPE); `systemd.unit(5)` `%d`; `systemd.exec(5)` `LoadCredential=` (local man pages, systemd 261)
- Go `os/signal` default behaviour: https://pkg.go.dev/os/signal
