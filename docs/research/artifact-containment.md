# Containing untrusted Artifacts on the Portal's origin

Research for [#2](https://github.com/yet-an-other/pub-hub/issues/2) (part of map #1). Date: 2026-09-27.

**Question.** Portal (authenticated UI + publish API) and Artifacts (arbitrary, untrusted HTML/JS) are meant to share `hub.bdgn.me`. What containment stops Artifact JS from acting with the owner's Portal session or tampering with other Artifacts? Is it enough, or should Artifacts move to a sibling host?

**Answer in one line.** Path scoping is no boundary. `CSP: sandbox` on the same FQDN does contain Artifacts, but it breaks storage and workers, and a single missing header hands over the owner's session. Serve Artifacts from their own host (`pub.bdgn.me`) and put Fetch Metadata checks in front of the Portal.

**Method.** Claims come from the specs and vendor docs listed under [Sources](#sources). The load-bearing ones were then checked in a throwaway lab: headless Chromium 151, a Go 1.27 server, and `hub.bdgn.test` / `pub.bdgn.test` mapped to loopback over HTTPS. `bdgn.test` is same-site in the same way `bdgn.me` is. Results are quoted as **[lab]**. Firefox and Safari were not tested.

---

## 1. Session cookies vs same-origin Artifact JS: is path scoping a boundary?

**No. For script on the same origin, `Path` is not a security boundary, and the cookie standard says so itself.**

- RFC 6265bis, §4.1.2.4 [The Path Attribute]: "Although seemingly useful for isolating cookies between different paths within a given host, the Path attribute cannot be relied upon for security." [6265bis-path]
- §8.5 Weak Confidentiality: "Cookies do not always provide isolation by path … Because some of these user agents (e.g., web browsers) do not isolate resources received from different paths, a resource retrieved from one path might be able to access cookies stored for another path." [6265bis-conf]
- §8.6 Weak Integrity: "the user agent will accept an arbitrary Path attribute in a Set-Cookie header field … servers SHOULD NOT both run mutually distrusting services on different paths of the same host and use cookies to store security-sensitive information." [6265bis-int]

The mechanics, for an Artifact at `https://hub.bdgn.me/pub/…` and the Portal at `/` and `/api/…`:

1. **Using the session does not require reading it.** `fetch('/api/…')` defaults to credentials mode `same-origin`, which "include[s] credentials with requests made to same-origin URLs" [fetch-creds]. The browser picks cookies by the *request* URL's path, not the page's. So `HttpOnly` and `Path=/api/` cookies are attached, and the response can be read. **[lab]** An unsandboxed page at `/pub/a/` fetched `/api/whoami`. The request carried `__Host-sess` (HttpOnly, Strict), `laxsess`, and `apipath` (`Path=/api/`), and the page read the JSON response.
2. **Reading path-scoped cookies.** Same-origin script can frame or open any Portal path and read `frameDocument.cookie`. **[lab]** An iframe of `/api/whoami` showed `apipath=secret` to the `/pub/a/` page. (`history.pushState` can also rewrite the document URL to any same-origin path, since the spec only compares scheme, host and port [html-rewrite]. Chromium 151 did not re-scope `document.cookie` after `pushState`, so the iframe route is the reliable one.)
3. **Driving the Portal UI.** A same-origin `window.open('/')` or iframe returns a scriptable window. Artifact JS can read the Portal's DOM and in-memory state (including any bearer token the SPA holds) and click its buttons.
4. **`SameSite` does nothing here.** A same-origin request is always same-site.
5. **`__Host-` forces `Path=/`.** A `__Host-` cookie "will have been set with a `Secure` attribute, a `Path` attribute with a value of `/`, and no `Domain` attribute" [6265bis-host]. The best-practice session cookie therefore cannot be path-scoped anyway. Its value is against *sibling hosts* (§5), not paths.
6. **The server cannot tell the two callers apart.** **[lab]** The unsandboxed Artifact's `POST /api/publish` arrived with `Sec-Fetch-Site: same-origin` and `Origin: https://hub.bdgn.test:8443`. Those are exactly the headers the Portal SPA sends, and Go's `http.CrossOriginProtection` allowed it. A `Referer` check fails too: the Artifact can suppress `Referer` or `pushState` to a Portal path first.
7. Concretely, with oauth2-proxy in front: same-origin Artifact JS can call `/oauth2/userinfo`, which returns the owner's email [o2p-endpoints]. If `--set-xauthrequest` and `--pass-access-token` are both enabled, `/oauth2/auth` also puts the IdP access token in an `X-Auth-Request-Access-Token` response header [o2p-config], and same-origin script can read that header.

**Conclusion.** On a shared origin, an unsandboxed Artifact *is* the Portal as far as the browser and server are concerned. Containment has to change the Artifact's origin: either an opaque origin via sandbox, or a different host.

## 2. `Content-Security-Policy: sandbox` on Artifact responses

CSP3: "The sandbox directive specifies an HTML sandbox policy which the user agent will apply to a resource, just as though it had been included in an iframe with a sandbox property." It "will be ignored entirely when delivered in a `Content-Security-Policy-Report-Only` header, or within a `meta` element" [csp-sandbox]. It must therefore be a real response header. Browser support: Baseline since 2016 [mdn-sandbox].

The key flag is the *sandboxed origin browsing context flag*, which is set unless `allow-same-origin` is present: "This flag forces content into an opaque origin, thus preventing it from accessing other content from the same origin. This flag also prevents script from reading from or writing to the `document.cookie` IDL attribute, and blocks access to `localStorage`" [html-sandbox-origin]. `allow-scripts` together with `allow-same-origin` on the Portal's own origin gives the Artifact back its real origin, so the sandbox protects nothing. (The HTML spec warns about the iframe equivalent: the embedded page can "simply remove the sandbox attribute and then reload itself" [html-iframe-sandbox].) **`allow-same-origin` is never acceptable on the Portal's FQDN.**

### What it protects (`sandbox allow-scripts …`, no `allow-same-origin`)

| Attack from Artifact JS | Result | Evidence |
|---|---|---|
| Read Portal API responses | Blocked. The Artifact's origin is `null`, so it is cross-origin and the Portal sends no CORS headers | **[lab]** `fetch('/api/whoami',{credentials:'include'})` → `TypeError: Failed to fetch` |
| Script a Portal window or frame | Blocked, because an opaque origin is never same-origin | [html-sandbox-origin] |
| Read or write cookies, storage | `document.cookie`, `localStorage`, IndexedDB throw `SecurityError` | [html-cookie], [html-localstorage], [storage-key]; **[lab]** |
| Blind CSRF (`no-cors` POST with credentials) | The request is sent with `Origin: null` and `Sec-Fetch-Site: cross-site`, so a Fetch Metadata check rejects it. **[lab]** Chromium attached no Lax/Strict cookies at all | [fetch-origin], [fetchmeta-site]; **[lab]** `CrossOriginProtection=DENIED` |
| Register a service worker | Impossible (see §3) | **[lab]** |
| Escape via popups | Popups inherit the sandbox ("sandbox propagates to auxiliary browsing contexts flag") unless `allow-popups-to-escape-sandbox` is set. Even then, every Artifact response carries its own header | [html-sandbox-aux] |
| Tamper with other Artifacts | Each sandboxed document gets a fresh opaque origin, so nothing is shared | [html-sandbox-origin] |

Caveat on cookies: RFC 6265bis computes a sandboxed top-level document's "site for cookies" from "the origin of `top-document`'s URI" rather than its opaque origin [6265bis-doc]. Read literally, the spec makes the Artifact's requests to the Portal *same-site*, so `SameSite=Strict` cookies would be attached. Chromium 151 attached none. Do not rely on either behaviour: the Portal must reject these requests itself (§5).

### What it breaks for typical SPAs and prototypes

| Feature | Behaviour under `sandbox allow-scripts` | Fix |
|---|---|---|
| `localStorage`, `sessionStorage` | Throw `SecurityError` [html-localstorage]. **[lab]** | None. Apps that touch storage at startup crash |
| IndexedDB, Cache API, anything using a storage key | Fail, because an opaque origin gives "failure" when obtaining a storage key [storage-key]. **[lab]** IndexedDB `SecurityError` | None |
| `document.cookie` | Throws `SecurityError` [html-cookie]. **[lab]** | None |
| `fetch('./data.json')` to own files | Becomes a cross-origin CORS request (`Origin: null`), which fails without `Access-Control-Allow-Origin`. **[lab]** | Send `Access-Control-Allow-Origin: *` on Artifact responses. That is enough because credentials are not included [fetch-cors-check]. **[lab]** status 200 |
| `<script type="module">` (all Vite builds), `import()`, `modulepreload`, `@font-face` | Fetched in `cors` mode [html-module-fetch], so blocked without ACAO. **[lab]** `Failed to fetch dynamically imported module` | Same `ACAO: *` fix. **[lab]** module loaded |
| Classic `<script src>`, `<img>`, CSS | Work (no-cors) | — |
| `new Worker(url)`, SharedWorker | Worker scripts are fetched with mode `same-origin` [html-classic-worker], which never matches an opaque origin. **[lab]** `SecurityError` | `blob:` workers only |
| Service workers | Unavailable (§3). PWA prototypes break | — |
| `history.pushState` / client-side routing | The spec allows it, since it compares scheme, host and port, not origin [html-rewrite]. **[lab]** it works in Chromium 151. Deep-link reloads still need server fallback, which has nothing to do with the sandbox | — |
| Forms, `alert`, `window.open`/`target=_blank`, downloads | Blocked unless `allow-forms`, `allow-modals`, `allow-popups`, `allow-downloads` are set [mdn-sandbox] | Add those tokens. They are harmless without `allow-same-origin` |

Net: rendered notes and simple prototypes work. Anything that keeps state (theme toggles, persisted stores, drafts), uses workers or service workers, or sets cookies breaks. The breakage is permanent: loosening it means adding `allow-same-origin`, which removes all protection.

### What must hold for the sandbox to be a boundary

1. **Every response that can render as a document from Artifact bytes carries the header.** That includes 404 and other error pages, `.svg`, `.xml`, `.xhtml`, `.html` inside Bundles, and mis-typed files. In nginx, `add_header` applies only to 200, 201, 204, 206, 301, 302, 303, 304, 307 and 308 unless `always` is given. It is also silently dropped when a nested block declares its own `add_header` [nginx-add-header]. Setting it in the single Go handler that emits Artifact bytes is safer. Add `X-Content-Type-Options: nosniff`.
2. **No other route ever serves Artifact bytes on the Portal origin.** That rules out any "raw", "download" or "preview" API that streams stored HTML, and any Portal preview built as `<iframe srcdoc=…>` or a `blob:` URL. Those inherit the Portal's origin.
3. **No service worker on the origin may control `/pub/`.** A service worker intercepts the *navigation request* before the response headers exist, so a worker whose scope covers `/pub/` could serve a replacement page without the sandbox header [sw-handle-fetch], [sw-window-client]. The Portal must not register one with scope `/`, and must never reflect user input as JavaScript (see §3).

Any single slip gives full Portal compromise (§1). That catastrophic failure mode is the main argument against this option.

## 3. Service worker registration by an Artifact

Rules, from the Service Workers spec:

- The registering page must be a secure context. Script and scope URLs must be `http(s)` (no `blob:`/`data:`), and both must be same-origin with the registering page. Otherwise it is `SecurityError` [sw-register].
- The script response must have a JavaScript MIME type. The **max scope** is the script's own directory unless the response carries `Service-Worker-Allowed` [sw-update]. **[lab]** `/pub/a/sw.js` with scope `/` gave `SecurityError … not under the max scope allowed ('/pub/a/')`. Scope `/pub/a/` succeeded.
- The spec itself says the path restriction "is not considered a hard security boundary, as only origins are. Sites are encouraged to use different origins to securely isolate segments of the site" [sw-path].
- Service worker script fetches carry `Service-Worker: script` [sw-script-request]. **[lab]** They also carry `Sec-Fetch-Dest: serviceworker`.

What this means for each layout:

- **Same FQDN, unsandboxed:** a Bundle can install a worker, but only for its own directory. It cannot intercept `/` or `/api/` unless the server sends `Service-Worker-Allowed`, or some Portal URL reflects attacker-controlled text as JavaScript at a higher path (the classic JSONP-style trick). This hardly matters, because same-origin script already has the Portal (§1). Workers matter here only for *persistence*.
- **Same FQDN, sandboxed:** registration is impossible. **[lab]** `Service worker is disabled because the context is sandboxed and lacks the 'allow-same-origin' flag`. CSP3 also blocks a worker whose own script response carries a sandbox policy [csp-sandbox].
- **Sibling host:** a Bundle's worker is confined to that Bundle's directory. **[lab]** `/x/sw.js` could not take scope `/`. Because "Artifacts never nest" (CONTEXT.md), a Bundle's directory contains only its own paths, so its worker cannot intercept other Artifacts. It can never touch `hub.bdgn.me`, which is a different origin.

**How to prevent it.** Never emit `Service-Worker-Allowed` for Artifact paths. For a cheap blanket ban, return 403 on any Artifact request carrying `Service-Worker: script` (nginx: `if ($http_service_worker) { return 403; }`). The ban also stops a worker from serving stale content after an Artifact is republished or deleted, which would otherwise undermine "republishing overwrites atomically".

## 4. Cross-Artifact interference on a shared origin

| Channel | Same FQDN + sandbox | Shared Artifact host, no sandbox |
|---|---|---|
| `localStorage` / IndexedDB / Cache | None (no storage) | Shared by all Artifacts. **[lab]** `/y/reader.html` read the value `/x/sibling.html` wrote |
| Cookies | None | Shared. Path does not isolate (§1) |
| DOM of each other's windows | Opaque origins, isolated | Same-origin: one Artifact can open another and script it in the reader's browser |
| Service workers | Impossible | Confined to the Bundle's own directory (§3), or banned outright |
| Server-side content | No effect (static, publish goes through the Portal API) | No effect |

**Does it matter here?** Mostly no. pub-hub is single-tenant, and Artifacts are public and static. The Artifact host has no auth and no secrets, and nothing can alter stored Artifacts except through the Portal API. The worst realistic case is that one Artifact reads or poisons another's `localStorage`, or rewrites another Artifact inside a reader's open tab. Accept it. If isolation between Artifacts is ever needed, per-Project hosts (`<project>.pub.bdgn.me`, wildcard cert) would give each Project its own origin. That is out of scope now.

## 5. Server-side defences on Portal endpoints

What the Portal sees from each kind of caller (all **[lab]**):

| Caller | `Sec-Fetch-Site` | `Origin` on POST | Cookies attached |
|---|---|---|---|
| Portal SPA | `same-origin` | Portal origin | all |
| Unsandboxed Artifact, same FQDN | `same-origin` | Portal origin | all (**indistinguishable**) |
| Sandboxed Artifact, same FQDN | `cross-site` | `null` | none in Chromium (the spec text says Strict/Lax may be sent, see §2) |
| Artifact on sibling `pub.` host | `same-site` | `https://pub…` | **all Lax/Strict cookies** (same site) |
| User typing a URL / bookmark | `none` | — | all |

Recommended defences:

1. **Fetch Metadata / Origin check on every state-changing request.** Go 1.25+ ships this as `net/http.CrossOriginProtection`. It rejects non-safe requests unless `Sec-Fetch-Site` is `same-origin` or `none`. When the header is absent, it falls back to comparing the `Origin` host with `Host`. Requests with neither header are treated as non-browser and allowed [go-cop], [go-cop-src]. It rejects `same-site`, so it covers the sibling-host case. Web.dev and OWASP both say to drop `same-site` from the allow-list when subdomains are not trusted [webdev-fm], [owasp-csrf]. `Sec-Fetch-Site` has been Baseline since March 2023 [mdn-sfs]. Behind nginx, forward the real host (`proxy_set_header Host $host`) so the `Origin` fallback compares correctly. **[lab]** Both the sandboxed (`cross-site`/`null`) and sibling (`same-site`) POSTs were denied. The same-origin unsandboxed POST was allowed, which is the §1 problem.
2. **GET/HEAD must have no side effects.** CrossOriginProtection always allows them, and Artifacts can trigger top-level GET navigations to any Portal URL (sign-out included).
3. **No cross-origin reads.** Never send CORS headers from the Portal. Add `Cross-Origin-Resource-Policy: same-origin` and `Cross-Origin-Opener-Policy: same-origin`, frame protection (`frame-ancestors 'none'`), and `nosniff`, following Post-Spectre Web Development [post-spectre]. Optionally, also reject `/api/` GETs whose `Sec-Fetch-Site` is not `same-origin`, the "resource isolation policy" [webdev-fm]. Check `event.origin` in any `postMessage` listener.
4. **Session cookie.** oauth2-proxy should set a `__Host-`-prefixed cookie name, no `--cookie-domain`, `--cookie-secure` (the default), and `--cookie-samesite=lax` (its default is empty) [o2p-config]. The `__Host-` prefix is what stops a sibling host forging or shadowing the Portal session. **[lab]** `document.cookie='__Host-sess=evil; Domain=bdgn.test…'` was rejected. A plain `tossed=evil; Domain=bdgn.test` cookie set from `pub.` *did* reach `hub.` (cookie tossing [6265bis-int], [gh-cookies]). Keep `--set-xauthrequest`/`--pass-access-token` off unless nginx really needs them. Exact flags belong to #3.
5. **Bearer tokens instead of cookies?** On the *same origin* they do not help. Artifact JS can call whatever endpoint mints the token using the session cookie, read the token out of a Portal window it opened, or read oauth2-proxy's header (§1.7). On a *separate origin*, the browser Portal is fine with the oauth2-proxy cookie plus check (1), and bearer tokens add token-storage complexity to the SPA. Bearer tokens *are* the right choice for machine Publishers (agents, CLI): browsers never attach them automatically, so they cannot be used for CSRF. The publish API should therefore accept either "cookie + Fetch Metadata" (browser) or `Authorization: Bearer` (machine). #7 decides the details.
6. **`Sec-Fetch-Dest`** is optional extra hardening. API endpoints can require `empty` (fetch/XHR) and refuse `document`/`iframe`/`serviceworker`. The Artifact host can refuse `serviceworker` the same way `Service-Worker: script` is refused (§3).

## 6. Reserved path prefix and future Cloudflare Tunnel exposure

Cloudflare Tunnel ingress rules are evaluated top-to-bottom by `hostname` and an optional `path` regex (Go RE2 syntax, so no look-ahead). The last rule must be a catch-all, typically `service: http_status:404`. cloudflared "forwards the full request path to your service without modifying or stripping it" [cf-ingress].

**Prefix layout 1: Artifacts under `/pub/…`, Portal at `/`.** This is an *allow-list*: `path: ^/pub/` goes to the origin and everything else gets 404. It fails closed, and it is also the single place where Artifact-only headers (sandbox, ACAO) are attached. Portal routes (`/api/`, oauth2-proxy's `/oauth2/`, `/favicon.ico`…) stay unexposed without anyone listing them.

**Prefix layout 2: Artifacts at root, Portal under a reserved prefix (e.g. `/_/`).** This is a *deny-list*. It needs ordered rules (`^/_/` → 404, then everything → origin) because RE2 has no negative look-ahead. It fails open: any Portal path outside the prefix is exposed, including oauth2-proxy's `/oauth2/` unless `--proxy-prefix` moves it [o2p-endpoints]. It also reserves Project names that clash with Portal routes. Worse than layout 1.

**Path-based exposure is fragile either way (derived from source and docs, not tested end-to-end).** cloudflared matches the rule against Go's `req.URL.Path` [cf-proxy-src], [cf-rule-src]. That value is percent-decoded but *not* dot-segment-normalised. Cloudflare's "Normalize URLs to origin" is **off** by default, so the origin receives the request unmodified [cf-norm]. nginx then matches locations "against a normalized URI, after decoding … resolving references to relative path components '.' and '..'" [nginx-location]. A raw request for `/pub/%2e%2e/api/publish` therefore plausibly passes `^/pub/` at the tunnel and lands on `/api/publish` in nginx. If the Portal ever shares an FQDN with public Artifacts, the tunnel must point at a **dedicated nginx `server`/listener that can only serve Artifacts**, and should not rely on the ingress path regex. Verify this before any path-based exposure.

**With a separate Artifact host, the question disappears.** The tunnel publishes `hostname: pub.bdgn.me` only, and `hub.bdgn.me` is never in the tunnel config (catch-all 404). Artifacts live at the root of their host (`https://pub.bdgn.me/xform/explanations/2026-09-12-explanation-xform-auth-seam.html`, `https://pub.bdgn.me/sicily-2025/`). Neither host needs a reserved prefix, and Project names cannot collide with Portal routes. `hub.bdgn.me/pub/…` can simply redirect. Naming rules stay with #5.

## 7. Options compared

| | A. Same FQDN, no containment | B. Same FQDN + `CSP: sandbox` | **C. Sibling host `pub.bdgn.me`** | D. Separate registrable domain |
|---|---|---|---|---|
| Artifact JS reads/acts as Portal session | **Yes, full takeover** | No, *if* the header is on every response | No (different origin) | No |
| Blind CSRF on Portal | Can't be stopped (same-origin) | Stopped by Fetch Metadata check | Stopped by Fetch Metadata check (same-site ≠ same-origin) | Stopped (cross-site; SameSite also helps) |
| Other `*.bdgn.me` services | Same-site with Artifacts | Same-site (see §2 caveat) | Same-site with Artifacts | **Cross-site** |
| Cookie tossing into Portal | n/a | n/a (no cookie access) | Possible; `__Host-` blocks session forgery, cookie-bomb DoS remains | Impossible |
| Renderer-process sharing (Spectre-class) | Same process | Same site, may share | Same site, may share [chromium-si] | Separate site |
| Artifact features (storage, workers, SW, modules) | All work | **Storage, workers and SW broken**; modules need ACAO `*` | All work (SW optionally banned) | All work |
| Failure mode on misconfiguration | — | **One missing header = full takeover** | Missing CSRF check = blind CSRF only; no reads | Same as C, lower impact |
| Public exposure via Tunnel | Unsafe | Path-based, fragile (§6) | Hostname-based, trivial | Hostname-based, trivial |
| Cost | none | none | +1 DNS name, cert SAN, nginx `server` | +1 domain and DNS zone (and Cloudflare zone for Tunnel), cert, nginx `server` |

## 8. Recommendation

**Overturn the same-FQDN preference and serve Artifacts from a sibling host, `pub.bdgn.me`. `hub.bdgn.me` becomes the Portal only.** Make the Artifact hostname a configuration value, so that moving to a separate registrable domain later (option D) needs no design change.

Concretely:

1. **Portal (`hub.bdgn.me`).** Wrap all state-changing handlers in `http.CrossOriginProtection`. Keep GETs side-effect free. Never send CORS headers. Send `CORP: same-origin`, `COOP: same-origin`, `frame-ancestors 'none'` and `nosniff`. The oauth2-proxy cookie is `__Host-`-named with no Domain and `SameSite=Lax`. Machine Publishers authenticate with bearer tokens, not cookies.
2. **Artifact host (`pub.bdgn.me`).** The nginx `server` block routes only to Artifact serving: no Portal API, no oauth2-proxy, no auth. Strip `Cookie` before proxying. Never send `Service-Worker-Allowed`. Return 403 to `Service-Worker: script` requests. Send `nosniff`. Serve a MIME type chosen by the server from the file extension (#4 decides the details). No CSP sandbox needed, so Artifacts keep storage, workers and modules.
3. **Invariant for the spec:** *Artifact bytes are only ever served from the Artifact host.* The Portal never serves or renders them on its own origin: no raw or preview route, no `srcdoc` and no `blob:` previews. Previews are cross-origin iframes of the `pub.` URL.
4. **Public exposure.** The Tunnel publishes `pub.bdgn.me` by hostname only. `hub.bdgn.me` stays LAN/VPN-only with no ingress rule.
5. **If the owner still insists on one FQDN,** option B is the only acceptable form, and all of these are required. Artifacts under `/pub/`. `Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups allow-popups-to-escape-sandbox allow-modals allow-downloads` plus `Access-Control-Allow-Origin: *` on *every* `/pub/` response, emitted by the one Artifact handler, never including `allow-same-origin`. The same Portal protections as in point 1. No Portal service worker whose scope covers `/pub/`. The Tunnel points at an nginx listener that serves only `/pub/`. Everyone involved accepts the §2 breakage.

### Residual risk with the recommendation (C)

- **Same-site with every `*.bdgn.me` service.** **[lab]** Artifact JS on `pub.` gets the owner's `SameSite=Lax/Strict` cookies attached to its requests to `hub.`, and the same holds for any other `*.bdgn.me` app (the IdP included, if it lives there). The Portal is covered by `CrossOriginProtection`. Any other service that relies on SameSite alone for CSRF protection is exposed to Artifacts. Only option D removes this.
- **Cookie tossing / cookie bombing.** Artifact JS can set `Domain=bdgn.me` cookies that reach every `*.bdgn.me` host. `__Host-` stops Portal session forgery, but a flood of large cookies can make the Portal (and siblings) reject requests until the cookies are cleared. This is a nuisance, not a takeover.
- **Process sharing.** Chrome's Site Isolation groups by site, and "This allows multiple origins within a site to share the same process" [chromium-si]. A Spectre-class leak from `pub.` into Portal data is only mitigated (CORP, COOP, ORB), not excluded.
- **Phishing on the owner's domain.** An Artifact can imitate the Portal or IdP login page under `bdgn.me`.
- **Cross-Artifact tampering** in the reader's browser (§4). Accepted.
- **Human error.** A future "preview" feature that renders Artifact HTML on `hub.` would reintroduce §1 in full. The invariant in point 3 exists to prevent that.

## New questions this surfaced (for the map)

- **Do other `*.bdgn.me` services (Zitadel, other home-lab apps) rely on `SameSite` for CSRF, or does oauth2-proxy use a shared `--cookie-domain=.bdgn.me`?** If either is true, choose option D, a separate registrable domain for Artifacts, over `pub.bdgn.me`.
- **Artifact host name and URL shape** (`pub.bdgn.me/<project>/…` vs keeping `/pub/`), and whether `hub.bdgn.me/pub/…` redirects. Feeds #5.
- **Service workers in Artifacts:** ban them (the recommended default) or allow them per Bundle? It affects republish semantics.
- **Sub-path asset URLs.** Vite's default `base: '/'` makes a Bundle at `/<project>/<bundle>/` request `/assets/…` from the host root. The publishing client or agent skill (#10) must build with relative `base`.

---

## Appendix: lab setup

A Go 1.27 server (TLS, self-signed, trusted via `--ignore-certificate-errors-spki-list`) routed by `Host`:

- `hub.bdgn.test` `/` sets `__Host-sess` (HttpOnly, Strict), `laxsess` (HttpOnly, Lax) and `apipath` (`Path=/api/`, Strict).
- `hub.bdgn.test` `/api/whoami` echoes the cookies and `Sec-Fetch-*` headers. `/api/publish` sits behind `http.CrossOriginProtection`.
- `hub.bdgn.test` serves `/pub/a/` (no CSP), `/pub/b/` (`sandbox allow-scripts` + `ACAO: *`) and `/pub/c/` (`sandbox allow-scripts` only).
- `pub.bdgn.test` serves `/x/sibling.html` and `/y/reader.html` (no CSP).

Headless Chromium 151.0.7922.173 used `--host-resolver-rules="MAP *.bdgn.test 127.0.0.1"` and one profile, visiting the Portal first so its cookies were set. Each page ran its probes and posted the JSON results back to the server log. Every **[lab]** statement above is taken from that log.

## Sources

- [6265bis-path] RFC 6265bis (draft, in RFC Editor queue), §4.1.2.4 The Path Attribute: https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#attribute-path
- [6265bis-host] RFC 6265bis, The "__Host-" Prefix: https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#the-host-prefix
- [6265bis-doc] RFC 6265bis, §5.2 "Same-site" and "cross-site" Requests / Document-based requests: https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#same-site-requests
- [6265bis-conf] RFC 6265bis, Weak Confidentiality: https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#weak-confidentiality
- [6265bis-int] RFC 6265bis, Weak Integrity: https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#weak-integrity
- [csp-sandbox] Content Security Policy Level 3, §6.3.2 sandbox: https://w3c.github.io/webappsec-csp/#directive-sandbox
- [mdn-sandbox] MDN, CSP: sandbox: https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Content-Security-Policy/sandbox
- [html-sandbox-origin] HTML Standard, sandboxed origin browsing context flag: https://html.spec.whatwg.org/multipage/browsers.html#sandboxed-origin-browsing-context-flag
- [html-sandbox-aux] HTML Standard, sandbox propagates to auxiliary browsing contexts flag: https://html.spec.whatwg.org/multipage/browsers.html#sandbox-propagates-to-auxiliary-browsing-contexts-flag
- [html-iframe-sandbox] HTML Standard, iframe `sandbox` attribute: https://html.spec.whatwg.org/multipage/iframe-embed-object.html#attr-iframe-sandbox
- [html-cookie] HTML Standard, `document.cookie`: https://html.spec.whatwg.org/multipage/dom.html#dom-document-cookie
- [html-localstorage] HTML Standard, `localStorage`: https://html.spec.whatwg.org/multipage/webstorage.html#dom-localstorage
- [html-rewrite] HTML Standard, "can have its URL rewritten": https://html.spec.whatwg.org/multipage/nav-history-apis.html#can-have-its-url-rewritten
- [html-classic-worker] HTML Standard, fetch a classic worker script (mode "same-origin"): https://html.spec.whatwg.org/multipage/webappapis.html#fetch-a-classic-worker-script
- [html-module-fetch] HTML Standard, fetch a single module script (mode "cors"): https://html.spec.whatwg.org/multipage/webappapis.html#fetch-a-single-module-script
- [storage-key] Storage Standard, obtain a storage key (opaque origin → failure): https://storage.spec.whatwg.org/#obtain-a-storage-key
- [fetch-creds] Fetch Standard, request credentials mode: https://fetch.spec.whatwg.org/#concept-request-credentials-mode
- [fetch-origin] Fetch Standard, `Origin` header: https://fetch.spec.whatwg.org/#origin-header
- [fetch-cors-check] Fetch Standard, CORS check: https://fetch.spec.whatwg.org/#cors-check
- [fetchmeta-site] Fetch Metadata Request Headers, `Sec-Fetch-Site`: https://w3c.github.io/webappsec-fetch-metadata/#sec-fetch-site-header
- [mdn-sfs] MDN, Sec-Fetch-Site (Baseline since March 2023): https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Sec-Fetch-Site
- [sw-register] Service Workers, Register algorithm: https://w3c.github.io/ServiceWorker/#register-algorithm
- [sw-update] Service Workers, Update algorithm (max scope, `Service-Worker-Allowed`, MIME check): https://w3c.github.io/ServiceWorker/#update-algorithm
- [sw-path] Service Workers, §6.5 Path restriction: https://w3c.github.io/ServiceWorker/#path-restriction
- [sw-script-request] Service Workers, Service worker script request (`Service-Worker: script`): https://w3c.github.io/ServiceWorker/#service-worker-script-request
- [sw-handle-fetch] Service Workers, Handle Fetch (navigation requests are routed to the controlling worker): https://w3c.github.io/ServiceWorker/#handle-fetch
- [sw-window-client] Service Workers, window client case: https://w3c.github.io/ServiceWorker/#control-and-use-window-client
- [go-cop] Go `net/http.CrossOriginProtection` (Go 1.25+): https://pkg.go.dev/net/http#CrossOriginProtection
- [go-cop-src] Go source, `src/net/http/csrf.go`: https://github.com/golang/go/blob/master/src/net/http/csrf.go
- [owasp-csrf] OWASP CSRF Prevention Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html
- [webdev-fm] web.dev, Protect your resources from web attacks with Fetch Metadata: https://web.dev/articles/fetch-metadata
- [post-spectre] W3C, Post-Spectre Web Development: https://www.w3.org/TR/post-spectre-webdev/
- [chromium-si] Chromium, Site Isolation: https://www.chromium.org/Home/chromium-security/site-isolation/
- [o2p-config] oauth2-proxy configuration overview (cookie and header options): https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview
- [o2p-endpoints] oauth2-proxy endpoints: https://oauth2-proxy.github.io/oauth2-proxy/features/endpoints
- [nginx-add-header] nginx `add_header`: https://nginx.org/en/docs/http/ngx_http_headers_module.html#add_header
- [nginx-location] nginx `location` (normalized URI matching): https://nginx.org/en/docs/http/ngx_http_core_module.html#location
- [cf-ingress] Cloudflare Tunnel configuration file, ingress rules: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/local-management/configuration-file/
- [cf-norm] Cloudflare URL normalization settings (Normalize URLs to origin: off by default): https://developers.cloudflare.com/rules/normalization/settings/
- [cf-proxy-src] cloudflared `proxy/proxy.go` (`FindMatchingRule(req.Host, req.URL.Path)`): https://github.com/cloudflare/cloudflared/blob/master/proxy/proxy.go
- [cf-rule-src] cloudflared `ingress/rule.go` (`Path.Regexp.MatchString(path)`): https://github.com/cloudflare/cloudflared/blob/master/ingress/rule.go
- [gh-cookies] GitHub Blog, "Yummy cookies across domains" (2013, why Pages moved to github.io): https://github.blog/news-insights/company-news/yummy-cookies-across-domains/
- Public Suffix List (`me` is a public suffix; `bdgn.me` is a registrable domain, so `hub.` and `pub.` are same-site): https://publicsuffix.org/list/public_suffix_list.dat
