# Public exposure of pub.bdgn.me via Cloudflare Tunnel

Research for [#13](https://github.com/yet-an-other/pub-hub/issues/13) (part of the map, #1). Researched on 2026-09-27 against primary sources:

- the Cloudflare developer docs (Cache, Speed, SSL, Rules, Bots, Tunnel);
- `cloudflared`, nginx and Ceph RGW source code, pinned to the commits read;
- the nginx.org module docs, RFC 9110 and RFC 9111, and `systemd.exec`.

Inline references such as [CF-occ] point to the [Sources](#sources).

Nothing was configured on a live zone or server. The sample `config.yml` was checked offline with `cloudflared tunnel ingress validate` (cloudflared 2026.9.3).

ADR 0001 and ADR 0002 are taken as given:

- **ADR 0001**: nginx serves Artifacts straight from S3, with `Cache-Control: no-cache`, the S3 ETag as validator, and the `Service-Worker: script` → `403` ban.
- **ADR 0002**: Artifacts live on the sibling host `pub.bdgn.me`, which the tunnel exposes by hostname only.

Changes that land in nginx, systemd or provisioning are inputs for [Deployment on the home server: units, nginx, secrets, provisioning](https://github.com/yet-an-other/pub-hub/issues/14).

## Answer in brief

- **Caching:**
  - By default Cloudflare never caches single-file Artifacts or Bundle roots: HTML and extensionless paths are not cache-eligible.
  - It does treat CSS, JS, images and fonts inside Bundles as cache-eligible. On Free, Pro and Business zones, Origin Cache Control is always on, and the docs say `no-cache` then means *store at the edge and revalidate on every request*. That is never stale, but it has three side effects:
    - edge copies of deleted files linger;
    - error responses without `Cache-Control` get cached (a `404` for 3 minutes);
    - whether the Reader's request headers reach nginx on a revalidation is undocumented.
  - **One Cache Rule, `http.host eq "pub.bdgn.me"` → Bypass cache, removes all of this, and no purge is ever needed.**
  - Also set the zone's Browser Cache TTL to *Respect Existing Headers*.
- **Conditional requests:**
  - `If-None-Match` travels unchanged to nginx and on to S3, and a `304` travels back. Cloudflare forwards every request header on uncached requests. nginx without `proxy_cache` never answers `304` itself.
  - **But** Cloudflare weakens the ETag (`W/"…"`) whenever it compresses. RGW compares `If-None-Match` byte-for-byte, so revalidations silently turn into full `200` responses.
  - **Fix:** nginx strips the `W/` prefix before proxying. Nothing is ever stale either way.
- **Compression:**
  - Cloudflare always asks the origin for `br, gzip` and compresses text itself for Readers (zstd on Free, Brotli on Pro and Business).
  - **Recommended:** nginx sends `Cache-Control: no-cache, no-transform`, and optionally gzips text itself. Cloudflare then passes Artifact bytes through unmodified.
- **Headers:**
  - `Service-Worker` (request), and `X-Robots-Tag` and `X-Content-Type-Options` (responses), pass through the edge and `cloudflared` untouched. The only response headers stripped are Cloudflare's `X-Accel-*` and `Alt-Svc`, and `cloudflared`'s own control headers.
  - The `403` ban needs the request to actually reach nginx. The Bypass rule guarantees that.
- **Features to turn off:** anything that rewrites Artifact HTML, or that keeps or advertises copies of Artifacts:
  - Always Online (hands URLs to the Internet Archive);
  - Crawler Hints (IndexNow);
  - Email Obfuscation (on by default);
  - Web Analytics auto-injection, Speed Brain, and "Replace insecure JavaScript libraries" (all on by default on Free);
  - Rocket Loader, Automatic HTTPS Rewrites, Cloudflare Fonts, AI Labyrinth, managed `robots.txt`, Bot Fight Mode, Markdown for Agents, and the "Add security headers" Managed Transform.
  - Auto Minify is retired.
- **cloudflared:**
  - Use a locally-managed tunnel. Its `config.yml` holds one `hostname: pub.bdgn.me` rule and a catch-all `http_status:404`.
  - The rule points at a **tunnel-only nginx listener** (`http://127.0.0.1:8081`) that carries nothing but the `pub.` server block.
  - Run it under its own hardened systemd unit: `DynamicUser`, credentials via `LoadCredential`, `--no-autoupdate`, updates through the package manager. `cloudflared service install` writes a root unit and optionally a self-update timer.
  - Network: outbound TCP/UDP to port 7844 only; no inbound ports.

## 1. The request path

```
Reader ──TLS──▶ Cloudflare edge for pub.bdgn.me (Universal SSL)
                  │ cache decision · feature rewrites · adds CF-Connecting-IP, X-Forwarded-For, CF-Ray …
                  ▼
           tunnel (QUIC, or HTTP/2 fallback), opened outbound by cloudflared to port 7844
                  ▼
           cloudflared on the home server — ingress: pub.bdgn.me → http://127.0.0.1:8081, anything else → 404
                  ▼
           nginx, tunnel-only listener → pub. server block (ADR 0001 rules) → Artifact bucket on RGW
```

`hub.bdgn.me` is absent at two layers:

- **cloudflared:** no ingress rule names it, so the catch-all answers `404` before nginx is contacted.
- **nginx:** the tunnel's listener holds no `hub.` server block.

## 2. The verification points

### 2.1 Does Cloudflare cache anything under `no-cache` + ETag? Are Cache Rules or a purge needed?

| Fact | Source |
|---|---|
| Cloudflare "only caches based on file extension and not by MIME type" and "does not cache HTML or JSON by default". The default list includes CSS, JS, SVG, PNG, JPG, WEBP, AVIF, GIF, ICO, WOFF, WOFF2, TTF, OTF, EOT, PDF, ZIP and more. | [CF-default-cache] |
| So single-file Artifacts (`….html`) and Bundle roots (`…/`) are never cache-eligible by default. Many files inside Bundles are. | derived |
| The default-behaviour page says Cloudflare does **not** cache when `Cache-Control` is `private`, `no-store`, `no-cache` or `max-age=0`. | [CF-default-cache] |
| Origin Cache Control (OCC) is "enabled by default" on Free, Pro and Business, and those plans "cannot disable it". With OCC on, `no-cache` means "Caches and always revalidates. Does not serve stale." | [CF-occ] |
| The cache-status reference agrees with the OCC table. With OCC on, `no-cache` and `max-age=0` "cause Cloudflare to cache and revalidate the response instead, producing `REVALIDATED` or `EXPIRED`". | [CF-cache-status] |
| Cache-eligible responses **without** `Cache-Control` get default edge TTLs: 200/206/301 for 120 minutes, 404/410 for 3 minutes. | [CF-default-cache] |
| "If a resource is cacheable and there is a cache miss, Cloudflare does not send ETag headers to the origin server." | [CF-etag] |
| A Cache Rule with **Bypass cache** yields `DYNAMIC`: "the request went to the origin web server without a cache lookup". Cache Rules exist on Free (10 rules). | [CF-cache-status], [CF-cache-rules] |
| Browser Cache TTL (default 4 hours) overrides the origin's `Cache-Control` when the origin value "is less than" the setting or is missing. It does not override under *Respect Existing Headers*. The docs do not say how it treats `no-cache`. A 2026 changelog only says the choice "depends on the underlying reason Cloudflare did not cache the response". | [CF-browser-ttl], [CF-bypass-changelog] |
| Purge by URL, hostname, tag or prefix, and purge everything, are available on all plans. | [CF-purge] |

**What this means without any rule:**

- **No stale `200`.** HTML is fetched from the origin every time. Cache-eligible Bundle files are revalidated with the origin on every request. So a republish or delete is visible to the next Reader request.
- **Edge copies of deleted Bundle files** remain in Cloudflare's cache until evicted. They are not served while the origin answers, but they exist, and Always Online (§2.4) could serve them when the origin is unreachable.
- **Error responses are cached** if they lack `Cache-Control` and the path has a cache-eligible extension. Example: a `404` for a deleted `…/app.js` is cached for 3 minutes, so republishing to that path stays invisible for up to 3 minutes. nginx's `add_header` applies only to 200, 201, 204, 206, 301, 302, 303, 304, 307 and 308 unless `always` is set [ngx-headers].
- **The ban on `.js` rests on undocumented behaviour.** A revalidation is Cloudflare's own conditional request, and no doc says it carries the Reader's `Service-Worker` header. Without a rule, the `403` ban for a cached `.js` depends on that.
- **Cloudflare's own pages contradict each other** on `no-cache`: "does not cache" versus "caches and always revalidates". Behaviour is best pinned with an explicit rule.

**Recommendation:** add one Cache Rule scoped to the hostname: `(http.host eq "pub.bdgn.me")` → **Bypass cache**. Then:

- nothing from `pub.` is stored at the edge;
- every request reaches nginx with all its headers;
- republish and delete need **no purge, ever**.

The only exception is a one-time "purge by hostname" if `pub.` was ever proxied before the rule existed.

nginx should still send `Cache-Control` on **every** status (`add_header … always`). Browsers act on it, and it is the fallback if the rule is ever removed.

**Browser side:** set the zone's Browser Cache TTL to **Respect Existing Headers**. The docs guarantee that setting stops Cloudflare inserting or overriding `Cache-Control`, and they do not say what the 4-hour default does to `no-cache`. This setting is zone-wide: other `*.bdgn.me` hostnames stop receiving Cloudflare's inserted `max-age`. Verify the result with the checks in §7.

**Cost:** `pub.` gets no CDN caching. Every Reader request crosses the tunnel to the home server. That matches ADR 0001's premise: Artifacts are short-lived and mostly viewed by their owner.

### 2.2 Do conditional requests (`If-None-Match` → `304`) and compression pass through?

**The revalidation chain works as long as the ETag reaches the origin unchanged:**

1. **Browser → Cloudflare.** On a `DYNAMIC` request, "Cloudflare passes all HTTP request headers to your origin web server" [CF-headers].
2. **cloudflared.** It clones the request with all its headers [cfd-proxy].
3. **nginx.** Without `proxy_cache`, nginx sets `r->disable_not_modified = !u->cacheable` [ngx-upstream]. Its not-modified filter is skipped when that flag is set [ngx-not-modified]. So nginx relays whatever S3 answers.
4. **RGW.** It unquotes `If-None-Match` and compares it with the stored ETag. On a match it returns `304` [rgw-rados]. `If-Modified-Since` is only evaluated when `If-None-Match` is absent [rgw-rados].
5. **The `304` travels back** through nginx, cloudflared and Cloudflare.

**Cloudflare weakens ETags.** Cloudflare turns a strong ETag into `W/"…"` whenever it compresses an uncompressed origin response, or recompresses into another encoding. It removes the ETag entirely when the value is malformed. Rocket Loader and Email Obfuscation also defeat strong ETags [CF-etag]. By default, Free zones compress to Readers with zstd, and Pro and Business with Brotli [CF-compression]. nginx's own gzip filter also weakens ETags, since nginx 1.7.3 [ngx-changes].

The browser then sends `If-None-Match: W/"<md5>"`:

- RFC 9110 requires weak comparison for `If-None-Match` [RFC9110].
- RGW's `rgw_string_unquote` only strips quotes when the value *starts* with `"` [rgw-common]. It then compares bytes, so `W/"…"` never matches and RGW returns a full `200` [rgw-rados].
- **Consequence:** never stale, but every revalidation of HTML, CSS, JS or SVG re-downloads the whole file through the tunnel.

**Fix (nginx, `pub.` server block):** normalise the header before proxying, using a regex `map` with a named capture [ngx-map]. This is safe because `If-None-Match` is defined with weak comparison anyway. When the client sends no `If-None-Match`, the mapped value is empty, and nginx does not send an empty header upstream [ngx-proxy].

```nginx
map $http_if_none_match $pub_if_none_match {
    default               $http_if_none_match;
    ~^W/(?<pub_inm>.+)$   $pub_inm;          # W/"etag" -> "etag"
}
# in the location that proxies to the Artifact bucket:
proxy_set_header If-None-Match $pub_if_none_match;
```

`proxy_set_header` is inherited only when the current level defines none [ngx-proxy]. So a location that sets any `proxy_set_header` must repeat this one.

**Compression, as documented:**

- **Cloudflare → origin:** Cloudflare always sends `Accept-Encoding: br, gzip` and overwrites the client's value [CF-headers], [CF-compression].
- **Cloudflare → Reader:**
  - It compresses a fixed list of text, font, SVG, JSON and wasm types.
  - Only statuses 200, 403 and 404 are compressed, and only above 48 bytes (gzip) or 50 bytes (Brotli, zstd).
  - An origin-compressed body passes through when the Reader accepts the same encoding; otherwise Cloudflare decompresses and re-encodes.
  - It may drop `Content-Length` when compressing [CF-compression].
- **`Cache-Control: no-transform` from the origin:** Cloudflare disables compression. "If the original asset fetched from the origin is compressed, it is served compressed to the visitor. If the original asset is uncompressed, compression is not applied" [CF-occ]. It also "prevents body changes by supported features" [CF-body-inspection]. RFC 9111 makes `no-transform` binding on intermediaries [RFC9111].

Two coherent setups:

| | A: Cloudflare compresses | B: byte-exact (recommended) |
|---|---|---|
| nginx sends | `Cache-Control: no-cache` | `Cache-Control: no-cache, no-transform`; optionally `gzip on` for text types |
| Reader receives | zstd, Brotli or gzip, chosen per Reader; weak ETag on compressed types | exactly nginx's bytes (gzip if nginx gzips, otherwise identity); `Content-Length` kept |
| Tunnel carries | identity bytes (unless nginx gzips) | gzip bytes, if nginx gzips |
| HTML rewriting prevented by | dashboard toggles only | the origin header for supported features, plus dashboard toggles |
| `W/` normalisation needed | yes | only if nginx gzips; harmless otherwise |

**Recommend B.** It makes "Readers get exactly the Publisher's bytes" a property of the origin, independent of toggles on a zone shared with other `*.bdgn.me` services. The cost is gzip instead of Brotli or zstd.

`no-transform` adds to ADR 0001's `no-cache`; it does not replace it, so it contradicts nothing.

### 2.3 Are the `Service-Worker` request header and the `X-Robots-Tag` / `X-Content-Type-Options` response headers preserved?

**Request direction:**

- **Cloudflare** "passes all HTTP request headers to your origin web server and adds additional headers" [CF-headers]:
  - It may drop only header names that are invalid by nginx's rules (for example, names containing a dot).
  - It overwrites `Accept-Encoding`, `Connection` and `X-Forwarded-Proto`.
  - It adds `CF-Connecting-IP`, `X-Forwarded-For`, `CF-Ray`, `CF-Visitor` and `CDN-Loop`.
- **`cloudflared`** clones the incoming request with every header [cfd-proxy]. It then only:
  - sets `Connection: keep-alive`;
  - sets an empty `User-Agent` when none was sent;
  - adds `Cf-Warp-Tag-*` headers when tags are configured;
  - replaces `Host`, moving the original to `X-Forwarded-Host`, only when `httpHostHeader` is configured [cfd-origin-proxy].
- **So `Service-Worker: script` reaches nginx and the `403` ban works for every request that reaches nginx.** With the Bypass rule, that is every request (§2.1).

**Response direction:**

- **Cloudflare** "passes all HTTP headers in the response from the origin server back to the visitor" except `X-Accel-Buffering`, `X-Accel-Charset`, `X-Accel-Limit-Rate`, `X-Accel-Redirect` and `Alt-Svc`. It adds `Cf-Ray` and `Cf-Cache-Status` [CF-headers].
- **`cloudflared`** forwards all origin response headers except its control headers: pseudo-headers, and names starting `cf-int-`, `cf-cloudflared-` or `cf-proxy-` [cfd-header].
- **So `X-Robots-Tag`, `X-Content-Type-Options`, `Content-Type` and `Cache-Control` arrive intact.**

Response headers Cloudflare *does* change or add:

- `ETag` is weakened (§2.2).
- `Content-Length` may be dropped when Cloudflare compresses [CF-compression].
- `Cache-Control` may be rewritten by Browser Cache TTL (§2.1).
- Speed Brain adds `Speculation-Rules` [CF-speed-brain].
- The "Add security headers" Managed Transform adds `x-frame-options: SAMEORIGIN`, `referrer-policy: same-origin`, `x-xss-protection` and `expect-ct` [CF-managed-transforms].
- Markdown for Agents keeps headers but replaces the body, and drops `ETag` and `Last-Modified` [CF-md-agents].

**Real client IP.** Through the tunnel, nginx sees the connection coming from `cloudflared` on loopback. Cloudflare recommends `CF-Connecting-IP` over `X-Forwarded-For` because it always holds exactly one address [CF-headers]. With `ngx_http_realip_module`, trust only the loopback peer: `set_real_ip_from 127.0.0.1; real_ip_header CF-Connecting-IP;` [ngx-realip].

The module is "not built by default" upstream, so check `nginx -V`. Do not enable the "Remove visitor IP headers" Managed Transform: it removes `CF-Connecting-IP` [CF-headers].

### 2.4 Which Cloudflare features should be off for Artifacts?

The authoritative list of features that change HTML bodies is in [CF-body-inspection]. Everything below changes Artifact bytes or headers, keeps copies, or advertises URLs.

| Feature | Default | What it does to Artifacts | Turn off at | Blocked by `no-transform`? |
|---|---|---|---|---|
| **Always Online** | opt-in toggle | When the origin is unreachable, serves stale cached copies or Internet Archive copies with a banner. Enabling it "shares your hostname and popular URL paths with the archive", whose crawler ignores `cache-control`. Deleted Artifacts could then outlive deletion in public. [CF-always-online], [CF-always-online-ts] | zone (Caching → Configuration) | no |
| **Crawler Hints** | opt-in | Sends URLs with cache status `MISS` to IndexNow search engines [CF-crawler-hints]. That advertises Artifact URLs. | zone (Caching → Configuration) | no |
| **Email Address Obfuscation** | "enables … automatically when you sign up" | Rewrites e-mail addresses in HTML and injects `email-decode.min.js` [CF-email-obf] | Configuration Rule, per hostname | yes, documented [CF-email-obf] |
| **Web Analytics / RUM auto-injection** | "Free customers have RUM enabled automatically, with EU traffic excluded" | Injects the beacon script into HTML [CF-rum], [CF-web-analytics] | Configuration Rule "Disable RUM" | yes, documented [CF-web-analytics] |
| **Speed Brain** | "Enabled by default" on Free | Adds a `Speculation-Rules` header that points at `/cdn-cgi/`. Prefetches are only answered from cache [CF-speed-brain]. | zone (Speed; API setting `speed_brain`) | no (it is a header) |
| **Replace insecure JavaScript libraries** | "turned on by default on Free plans" | Rewrites `<script src>` for known libraries to cdnjs [CF-replace-js]. "you cannot turn it off using Configuration Rules" [CF-compression]. | zone (Security settings) | general statement only [CF-body-inspection] |
| **Rocket Loader** | opt-in | Rewrites how scripts load [CF-rocket-loader] | Configuration Rule | general statement only |
| **Automatic HTTPS Rewrites** | — | Rewrites `http://` links in HTML [CF-https-rewrites] | Configuration Rule | general statement only |
| **Cloudflare Fonts** | opt-in | Replaces Google Fonts links with inline CSS [CF-fonts] | Configuration Rule | general statement only |
| **Polish** (Pro+), Mirage (deprecated) | opt-in | Re-encodes images | Configuration Rule (Polish) | Polish: yes [CF-occ] |
| **Bot Fight Mode** (+ JavaScript Detections) | opt-in | Challenges bot-like clients, including agents and CLIs fetching Artifacts. WAF custom rules and Page Rules cannot bypass or skip it, and it forces JavaScript Detections on, which injects scripts into HTML [CF-bot-fight], [CF-js-detections]. | zone (Security → Bot traffic) | JS injection: yes [CF-occ]; challenges: no |
| **AI Labyrinth** | opt-in | Injects hidden links into HTML [CF-ai-labyrinth] | zone (Bot traffic) | general statement only |
| **Managed `robots.txt`** | opt-in | Serves or prepends a Cloudflare `robots.txt` [CF-managed-robots] | zone | no |
| **Markdown for Agents** | opt-in (Pro, Business) | Returns Artifact HTML converted to Markdown for `Accept: text/markdown` [CF-md-agents] | Configuration Rule / AI Crawl Control | — |
| **Managed Transform "Add security headers"** | opt-in | Adds `X-Frame-Options: SAMEORIGIN` and more [CF-managed-transforms] | zone (Rules → Managed Transforms) | no |
| **Hotlink Protection** | opt-in | Blocks image requests that carry a foreign `Referer` [CF-hotlink] | Configuration Rule | no |
| **Auto Minify** | retired 2024-08-05 | — | confirm `css/html/js: off` via the API [CF-auto-minify] | — |
| **Early Hints** | opt-in | Caches `Link` headers from HTML. The `pub.` responses carry none. [CF-early-hints] | leave off | — |

Keep **Always Use HTTPS** on: it is an edge redirect and does not touch the body. Universal SSL covers `pub.bdgn.me` because it is a first-level subdomain, and the record must be proxied [CF-universal-ssl]. The zone's SSL/TLS encryption mode does not apply to the `cloudflared` → origin hop [CF-tunnel-https-origins].

### 2.5 cloudflared service and ingress essentials, on a host without Docker

**Locally- vs remotely-managed.** Cloudflare recommends remotely-managed tunnels for most uses. It describes locally-managed ones, whose configuration lives in `config.yml` on the host, as meant for "specific scenarios" [CF-tunnel-lm].

For pub-hub, the invariant "`hub.` never appears in the tunnel config" is best held in a file under the owner's configuration management on the host. So use a locally-managed tunnel. A remotely-managed tunnel works the same way: the same two routes go in the dashboard, and the token is passed with `--token-file` [CF-tunnel-tokens], [CF-tunnel-run-params].

**Ingress:**

- Rules are evaluated top to bottom. A rule without `hostname` matches every host, and the last rule must be a catch-all. `http_status:404` answers from `cloudflared` itself [CF-tunnel-configfile].
- `cloudflared` "forwards the full request path to your service without modifying or stripping it" [CF-tunnel-configfile], [CF-tunnel-terms].
- Validate with `cloudflared tunnel ingress validate`, and test a URL with `cloudflared tunnel ingress rule <url>` [CF-tunnel-configfile]. Offline check of the §5 file: `https://hub.bdgn.me/api/artifacts` → "Matched rule #1 … http_status:404".

**DNS:**

- `cloudflared tunnel route dns <tunnel> pub.bdgn.me` creates a `CNAME` to `<UUID>.cfargotunnel.com` [CF-tunnel-create].
- The `cfargotunnel.com` name only proxies records in the same account.
- A stopped tunnel leaves the record in place, and Readers get error `1016` [CF-tunnel-routing].
- Published hostnames inherit the zone's Cache Rules, WAF and other rules for that hostname [CF-tunnel-routing].

**Origin.** Point the rule at a **tunnel-only nginx listener**, for example `http://127.0.0.1:8081`, where only the `pub.` server block listens:

- `cloudflared` passes the Reader's `Host` through unless `httpHostHeader` is set [CF-tunnel-origin-params], [cfd-origin-proxy]. With a listener that only `pub.` uses, reaching `pub.` alone becomes a property of nginx's socket layout rather than of the `Host` header.
- Plain HTTP on loopback is fine. The edge ↔ `cloudflared` leg is encrypted independently of the zone's SSL/TLS mode, and that mode does not validate or select the local protocol [CF-tunnel-https-origins].
- `service: unix:/path` also works [CF-tunnel-configfile]. nginx accepts `set_real_ip_from unix:` [ngx-realip]. Access is then controlled by the socket directory's permissions.

**nginx consequence of a plain-HTTP loopback listener.** nginx's redirects are absolute by default [ngx-core]:

- They use `absolute_redirect on`, the `Host` header, and `port_in_redirect on`.
- So the Bundle-root trailing-slash `301` would say `Location: http://pub.bdgn.me:8081/<project>/<name>/`.
- **Set `absolute_redirect off`** to emit a relative `Location` [ngx-core].

**Credentials:**

- `cloudflared tunnel login` writes `cert.pem`. It is account-wide: it can create, delete and route all tunnels in the account. It is valid for at least 10 years, and its token lasts until revoked.
- `cert.pem` is needed only to create the tunnel and its DNS route, so keep it off the server.
- The tunnel credentials file `<UUID>.json` is tunnel-scoped. It "does not expire" and can only run this tunnel [CF-tunnel-perms].

**Network:**

- `cloudflared` connects outbound to port 7844 over TCP/UDP. The protocol defaults to `quic` with `http2` fallback (`--protocol`) [CF-tunnel-config], [CF-tunnel-run-params].
- Block all ingress. Nothing listens publicly.
- For quic-go's "failed to sufficiently increase receive buffer size" warning, raise `net.core.rmem_max` [CF-tunnel-troubleshoot].

**Metrics.** Prometheus metrics are served on `127.0.0.1:20241`–`20245` by default, or on the `--metrics` address [CF-tunnel-observability]. This is relevant to [Backups and observability](https://github.com/yet-an-other/pub-hub/issues/15).

**Install and update:**

- Install from Cloudflare's apt or rpm repository (pkg.cloudflare.com), or from the distribution's package [CF-tunnel-create].
- Automatic updates do not apply when `cloudflared` "was installed by a package manager" [CF-tunnel-run-params], so update through the package manager.
- `cloudflared service install` writes `/etc/systemd/system/cloudflared.service`. That unit runs as **root**, with `Type=notify`, `--no-autoupdate`, `--config /etc/cloudflared/config.yml tunnel run` and `Restart=on-failure`.
- Unless `--no-update-service` is passed, it also installs a daily `cloudflared-update.timer` that self-updates the binary and restarts the tunnel [cfd-linux-service].
- Prefer the hand-written unit in §6.

## 3. Recommended Cloudflare settings (checklist)

**Hostname-scoped, for `pub.bdgn.me`:**

- [ ] **DNS:** `pub` → `CNAME <UUID>.cfargotunnel.com`, **Proxied**. Create it with `cloudflared tunnel route dns pub-hub pub.bdgn.me`. No public record for `hub` may point at the tunnel. The catch-all `404` is the backstop.
- [ ] **Cache Rule** "pub-hub: no edge cache": `(http.host eq "pub.bdgn.me")` → Cache eligibility **Bypass cache**.
- [ ] **Configuration Rule** "pub-hub: Artifacts unmodified": `(http.host eq "pub.bdgn.me")` →
  - Automatic HTTPS Rewrites **Off**
  - Email Obfuscation **Off**
  - Rocket Loader **Off**
  - Fonts **Off**
  - Polish **Off**
  - Hotlink Protection **Off**
  - Markdown for Agents **Off**
  - **Disable RUM**
  - **Disable Zaraz**

  All of these are Configuration Rule settings [CF-config-rules-settings]. The Free plan allows 10 Configuration Rules [CF-config-rules].

**Zone-wide, on `bdgn.me`.** These affect every proxied `*.bdgn.me` hostname, so check each against the other services:

- [ ] **Always Online:** Off.
- [ ] **Crawler Hints:** Off.
- [ ] **Browser Cache TTL:** Respect Existing Headers.
- [ ] **Speed Brain:** Off.
- [ ] **Replace insecure JavaScript libraries:** Off. It cannot be scoped by hostname.
- [ ] **Bot Fight Mode:** Off, or accept that non-browser Readers get challenged. **AI Labyrinth:** Off. **Managed robots.txt:** Off.
- [ ] **Managed Transforms:** "Add security headers" Off. Leave "Remove visitor IP headers" Off, because nginx logs need `CF-Connecting-IP`.
- [ ] **Auto Minify:** `GET /zones/{zone_id}/settings/minify` shows `css/html/js` all `off` [CF-auto-minify].
- [ ] **Always Use HTTPS:** On.
- [ ] **One-time:** only if `pub.bdgn.me` was proxied before the Cache Rule existed, purge by hostname `pub.bdgn.me` [CF-purge].

## 4. Origin changes this implies (input for the deployment ticket)

Illustrative only. The `pub.` server block itself belongs to [Deployment on the home server: units, nginx, secrets, provisioning](https://github.com/yet-an-other/pub-hub/issues/14).

```nginx
# http {} level
map $http_if_none_match $pub_if_none_match {
    default               $http_if_none_match;
    ~^W/(?<pub_inm>.+)$   $pub_inm;           # Cloudflare/nginx-weakened ETag -> what RGW can match
}

server {
    server_name pub.bdgn.me;
    listen 443 ssl;                  # LAN/VPN Readers, TLS as decided for the deployment
    listen 127.0.0.1:8081;           # cloudflared only; no other server block listens here

    absolute_redirect off;           # 301 Location: /<project>/<name>/, not http://pub.bdgn.me:8081/…

    set_real_ip_from 127.0.0.1;      # only cloudflared connects over loopback
    real_ip_header   CF-Connecting-IP;

    add_header Cache-Control "no-cache, no-transform" always;   # every status: 200, 301, 403, 404
    # … X-Robots-Tag and X-Content-Type-Options (always), Service-Worker: script -> 403,
    #   exact key -> dotless-miss 301 -> …/ index.html, S3 403 -> 404, hide x-amz-* (ADR 0001) …

    location / {
        proxy_set_header If-None-Match $pub_if_none_match;   # repeat every proxy_set_header here
        # proxy_pass to the Artifact bucket …
    }

    # Optional: saves home upload bandwidth; Cloudflare passes gzip through under no-transform.
    gzip on;                         # text/html is always included [ngx-gzip]
    gzip_types text/css text/javascript application/javascript application/json
               image/svg+xml text/plain application/wasm;
}
```

## 5. Sample `cloudflared` `config.yml`

This file validated with `cloudflared tunnel --config config.yml ingress validate` → `OK`. `ingress rule https://pub.bdgn.me/proj/notes/plan.html` → rule #0, and `ingress rule https://hub.bdgn.me/api/artifacts` → rule #1 (`http_status:404`).

```yaml
# /etc/cloudflared/config.yml — locally-managed tunnel exposing only pub.bdgn.me
tunnel: 6ff42ae2-765d-4adf-8112-31c55c1551ef   # tunnel UUID (an identifier, not a secret)
# credentials-file comes from TUNNEL_CRED_FILE, set by the systemd unit via LoadCredential=

originRequest:                 # defaults for every rule below
  connectTimeout: 10s

ingress:
  # The only published hostname. It goes to nginx's tunnel-only listener,
  # which carries nothing but the pub. server block.
  - hostname: pub.bdgn.me
    service: http://127.0.0.1:8081
  # Catch-all: any other hostname routed to this tunnel (hub.bdgn.me included)
  # gets a 404 from cloudflared and never reaches nginx.
  - service: http_status:404
```

## 6. systemd setup essentials

**One-time provisioning, as the owner:**

```sh
cloudflared tunnel login                          # writes the account-wide cert.pem; keep it off the server
cloudflared tunnel create pub-hub                 # writes <UUID>.json (tunnel-scoped credentials)
cloudflared tunnel route dns pub-hub pub.bdgn.me  # CNAME pub -> <UUID>.cfargotunnel.com
install -m 0600 -o root -g root <UUID>.json /etc/cloudflared/pub-hub-tunnel.json   # on the server
cloudflared tunnel --config /etc/cloudflared/config.yml ingress validate
```

**The unit.** It mirrors the upstream template [cfd-linux-service] (`Type=notify`, `--no-autoupdate`, `Restart=on-failure`), but runs unprivileged and reads the credentials through systemd credentials:

- `LoadCredential=` exposes the file read-only to the unit's user only.
- `%d` expands to the credentials directory in `Environment=` [systemd-exec].
- `cloudflared` reads `TUNNEL_CRED_FILE` as `--credentials-file` [cfd-subcommands].

```ini
# /etc/systemd/system/cloudflared-pub-hub.service
[Unit]
Description=Cloudflare Tunnel for pub.bdgn.me
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
DynamicUser=yes
LoadCredential=tunnel.json:/etc/cloudflared/pub-hub-tunnel.json
Environment=TUNNEL_CRED_FILE=%d/tunnel.json
ExecStart=/usr/bin/cloudflared --no-autoupdate --config /etc/cloudflared/config.yml tunnel run
Restart=on-failure
RestartSec=5s
TimeoutStartSec=15
# hardening (verify cloudflared still starts under each)
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK

[Install]
WantedBy=multi-user.target
```

**Essentials:**

- No inbound firewall openings are needed. Allow egress TCP/UDP 7844 [CF-tunnel-config].
- Update with the package manager. Do not install the self-update timer.
- After changing `config.yml`, run `ingress validate` and restart the unit [CF-tunnel-linux].
- `cloudflared tunnel info pub-hub` shows the connectors [CF-tunnel-create].

## 7. Post-deployment verification

These are read-only checks against the live hostname. `<p>/<b>/` is any published Bundle.

```sh
# Not cached at the edge; origin headers intact
curl -sI https://pub.bdgn.me/<p>/<b>/app.js | grep -iE 'cf-cache-status|cache-control|x-robots-tag|x-content-type-options|etag'
#   expect: cf-cache-status: DYNAMIC · cache-control: no-cache, no-transform · both X- headers · an etag

# Service-worker ban reaches nginx
curl -s -o /dev/null -w '%{http_code}\n' -H 'Service-Worker: script' https://pub.bdgn.me/<p>/<b>/app.js   # 403

# Revalidation returns 304, with the plain and the weakened tag
curl -s -o /dev/null -w '%{http_code}\n' -H 'If-None-Match: "<etag>"'   https://pub.bdgn.me/<p>/<b>/app.js  # 304
curl -s -o /dev/null -w '%{http_code}\n' -H 'If-None-Match: W/"<etag>"' https://pub.bdgn.me/<p>/<b>/app.js  # 304

# Relative trailing-slash redirect
curl -sI https://pub.bdgn.me/<p>/<b> | grep -i '^location'        # location: /<p>/<b>/

# Bytes unmodified (compare with the published file)
curl -s --compressed https://pub.bdgn.me/<p>/<name>.html | sha256sum

# 404 for a missing cache-eligible path is not cached
curl -sI https://pub.bdgn.me/<p>/<b>/missing.css | grep -iE '^HTTP|cf-cache-status|cache-control'
```

## 8. Residual caveats

- **Availability.** When the home server or tunnel is down, Readers get Cloudflare error `1016` [CF-tunnel-routing]. With Always Online off and the Bypass rule, nothing stale is served in its place. That is consistent with ADR 0001: deletes are immediate and final.
- **Cloudflare sees plaintext.** The edge terminates TLS, so Cloudflare sees every Artifact and every Reader request. This is inherent to the tunnel.
- **`/cdn-cgi/` belongs to Cloudflare** on every proxied hostname. It "cannot be modified or customized" [CF-cdn-cgi]. The segment rule `^[a-z0-9]+(-[a-z0-9]+)*$` allows a Project named `cdn-cgi`, whose Artifacts would be unreachable publicly.
  - Suggest reserving `cdn-cgi` alongside `index`.
  - This amends the naming rules from [Artifact host, URL layout and naming rules](https://github.com/yet-an-other/pub-hub/issues/5), so flag it for [Assemble the handoff spec](https://github.com/yet-an-other/pub-hub/issues/16).
- **`/robots.txt` on a Free zone.** When the origin has no `robots.txt` and managed `robots.txt` is off, Free zones "display the Content Signals Policy" [CF-managed-robots]. So `pub.bdgn.me/robots.txt` returns Cloudflare's comment-only file, not nginx's `404`.
  - It contains no `Disallow`, so the reason for having no `robots.txt` still holds: a `Disallow` would hide `X-Robots-Tag` from crawlers.
  - The documented opt-out ("Display Content Signals Policy") is described for the managed file. Whether it also applies here is unclear.
- **Cloudflare's docs contradict each other** on `no-cache` under Origin Cache Control, and are silent on how Browser Cache TTL treats `no-cache`. The Bypass rule and *Respect Existing Headers* sidestep both. The §7 checks confirm the actual behaviour.
- **RGW version.** RGW behaviour is from the source on `main`. The deployed Ceph version should be checked for the same `If-None-Match` handling; see [S3 client for the deployed Ceph RGW](https://github.com/yet-an-other/pub-hub/issues/12).
- **Shared zone.** Zone-wide toggles (Always Online, Crawler Hints, Browser Cache TTL, Speed Brain, Replace insecure JS, Bot Fight Mode, Managed Transforms) also change `kuber.bdgn.me` and every other proxied `*.bdgn.me` host.
- **Locally-managed tunnels** are described by Cloudflare as meant for specific scenarios [CF-tunnel-lm]. If that support is ever reduced, the same two routes move to a remotely-managed tunnel unchanged.
- **Spoofable client IP.** Any local process that can connect to `127.0.0.1:8081` can forge `CF-Connecting-IP`. That only affects logs. A `unix:` socket with restrictive directory permissions closes this.
- **If `hub.` is ever exposed** (not planned), Cloudflare's request-body limit is 100 MB on Free and Pro [CF-default-cache]. That exactly equals the Publish API's 100 MB limit, so multipart overhead would push maximum-size publishes over it.

## Sources

Cloudflare documentation (fetched 2026-09-27):

- [CF-default-cache] Default cache behavior — https://developers.cloudflare.com/cache/concepts/default-cache-behavior/
- [CF-occ] Origin Cache Control — https://developers.cloudflare.com/cache/concepts/cache-control/
- [CF-cache-status] Cloudflare cache responses — https://developers.cloudflare.com/cache/concepts/cache-responses/
- [CF-bypass-changelog] BYPASS status now returned for uncacheable responses (2026-05-26) — https://developers.cloudflare.com/changelog/ (source: https://github.com/cloudflare/cloudflare-docs/blob/production/src/content/changelog/cache/2026-05-26-bypass-status-for-uncacheable-responses.mdx)
- [CF-etag] Using ETag Headers with Cloudflare — https://developers.cloudflare.com/cache/reference/etag-headers/
- [CF-browser-ttl] Set Browser Cache TTL / Edge and Browser Cache TTL — https://developers.cloudflare.com/cache/how-to/edge-browser-cache-ttl/set-browser-ttl/ , https://developers.cloudflare.com/cache/how-to/edge-browser-cache-ttl/
- [CF-cache-rules] Cache Rules — https://developers.cloudflare.com/cache/how-to/cache-rules/ ; settings: https://developers.cloudflare.com/cache/how-to/cache-rules/settings/
- [CF-purge] Purge cache — https://developers.cloudflare.com/cache/how-to/purge-cache/
- [CF-always-online] Always Online — https://developers.cloudflare.com/cache/how-to/always-online/
- [CF-always-online-ts] Always Online troubleshooting — https://developers.cloudflare.com/cache/troubleshooting/always-online/
- [CF-crawler-hints] Crawler Hints — https://developers.cloudflare.com/cache/advanced-configuration/crawler-hints/
- [CF-early-hints] Early Hints — https://developers.cloudflare.com/cache/advanced-configuration/early-hints/
- [CF-compression] Content compression — https://developers.cloudflare.com/speed/optimization/content/compression/
- [CF-headers] Cloudflare HTTP headers — https://developers.cloudflare.com/fundamentals/reference/http-headers/
- [CF-body-inspection] Response body inspection — https://developers.cloudflare.com/rules/configuration-rules/response-body-inspection/
- [CF-config-rules] Configuration Rules — https://developers.cloudflare.com/rules/configuration-rules/
- [CF-config-rules-settings] Configuration Rules settings — https://developers.cloudflare.com/rules/configuration-rules/settings/
- [CF-email-obf] Email Address Obfuscation — https://developers.cloudflare.com/waf/tools/scrape-shield/email-address-obfuscation/
- [CF-hotlink] Hotlink Protection — https://developers.cloudflare.com/waf/tools/scrape-shield/hotlink-protection/
- [CF-rum] RUM beacon for Web Analytics — https://developers.cloudflare.com/speed/observatory/rum-beacon/
- [CF-web-analytics] Web Analytics get started — https://developers.cloudflare.com/web-analytics/get-started/
- [CF-speed-brain] Speed Brain — https://developers.cloudflare.com/speed/optimization/content/speed-brain/
- [CF-rocket-loader] Rocket Loader — https://developers.cloudflare.com/speed/optimization/content/rocket-loader/
- [CF-fonts] Cloudflare Fonts — https://developers.cloudflare.com/speed/optimization/content/fonts/
- [CF-auto-minify] Turn off Auto Minify via API — https://developers.cloudflare.com/speed/optimization/content/troubleshooting/disable-auto-minify/
- [CF-replace-js] Replace insecure JavaScript libraries — https://developers.cloudflare.com/waf/tools/replace-insecure-js-libraries/
- [CF-https-rewrites] Automatic HTTPS Rewrites — https://developers.cloudflare.com/ssl/edge-certificates/additional-options/automatic-https-rewrites/
- [CF-universal-ssl] Universal SSL limitations — https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/
- [CF-bot-fight] Bot Fight Mode — https://developers.cloudflare.com/bots/get-started/bot-fight-mode/
- [CF-js-detections] JavaScript Detections — https://developers.cloudflare.com/cloudflare-challenges/challenge-types/javascript-detections/
- [CF-ai-labyrinth] AI Labyrinth — https://developers.cloudflare.com/bots/additional-configurations/ai-labyrinth/
- [CF-managed-robots] robots.txt setting — https://developers.cloudflare.com/bots/additional-configurations/managed-robots-txt/
- [CF-md-agents] Markdown for Agents — https://developers.cloudflare.com/fundamentals/reference/markdown-for-agents/
- [CF-managed-transforms] Managed Transforms reference — https://developers.cloudflare.com/rules/transform/managed-transforms/reference/
- [CF-cdn-cgi] /cdn-cgi/ endpoint — https://developers.cloudflare.com/fundamentals/reference/cdn-cgi-endpoint/
- [CF-tunnel-configfile] Tunnel configuration file — https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/configuration-file/
- [CF-tunnel-origin-params] Origin parameters — https://developers.cloudflare.com/tunnel/reference/origin-parameters/
- [CF-tunnel-run-params] Run parameters — https://developers.cloudflare.com/tunnel/reference/run-parameters/
- [CF-tunnel-https-origins] Troubleshoot HTTPS origins — https://developers.cloudflare.com/tunnel/troubleshooting/https-origins/
- [CF-tunnel-linux] Run as a service on Linux — https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/as-a-service/linux/
- [CF-tunnel-create] Create a locally-managed tunnel — https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/create-local-tunnel/
- [CF-tunnel-lm] Locally-managed tunnels — https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/
- [CF-tunnel-perms] Tunnel permissions — https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/tunnel-permissions/
- [CF-tunnel-terms] Useful terms — https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/local-tunnel-terms/
- [CF-tunnel-tokens] Tunnel tokens — https://developers.cloudflare.com/tunnel/reference/tunnel-tokens/
- [CF-tunnel-routing] Routing — https://developers.cloudflare.com/tunnel/concepts/routing/
- [CF-tunnel-config] Tunnel configuration (firewall rules) — https://developers.cloudflare.com/tunnel/configuration/
- [CF-tunnel-observability] Observability (metrics) — https://developers.cloudflare.com/tunnel/observability/
- [CF-tunnel-troubleshoot] Troubleshooting — https://developers.cloudflare.com/tunnel/troubleshooting/

Source code (pinned):

- [cfd-proxy] cloudflared `proxy/proxy.go` (ingress match l.135; request clone l.249; header changes l.266–272) — https://github.com/cloudflare/cloudflared/blob/f9676c585623c86c0a48dbb6ae80840b4c834718/proxy/proxy.go
- [cfd-origin-proxy] cloudflared `ingress/origin_proxy.go` (`httpService.RoundTrip`, l.35–58) — https://github.com/cloudflare/cloudflared/blob/f9676c585623c86c0a48dbb6ae80840b4c834718/ingress/origin_proxy.go
- [cfd-header] cloudflared `connection/header.go` (`IsControlResponseHeader`, l.53) — https://github.com/cloudflare/cloudflared/blob/f9676c585623c86c0a48dbb6ae80840b4c834718/connection/header.go
- [cfd-linux-service] cloudflared `cmd/cloudflared/linux_service.go` (systemd templates, l.73–112) — https://github.com/cloudflare/cloudflared/blob/f9676c585623c86c0a48dbb6ae80840b4c834718/cmd/cloudflared/linux_service.go
- [cfd-subcommands] cloudflared `cmd/cloudflared/tunnel/subcommands.go` (`credentials-file` / `TUNNEL_CRED_FILE`, l.116–121) — https://github.com/cloudflare/cloudflared/blob/f9676c585623c86c0a48dbb6ae80840b4c834718/cmd/cloudflared/tunnel/subcommands.go
- [ngx-upstream] nginx `src/http/ngx_http_upstream.c` l.3211 — https://github.com/nginx/nginx/blob/939334efff3575ce52597cc8c13d55821044ac57/src/http/ngx_http_upstream.c#L3211
- [ngx-not-modified] nginx `ngx_http_not_modified_filter_module.c` (l.59; weak comparison l.172–216) — https://github.com/nginx/nginx/blob/939334efff3575ce52597cc8c13d55821044ac57/src/http/modules/ngx_http_not_modified_filter_module.c
- [rgw-rados] Ceph `src/rgw/driver/rados/rgw_rados.cc`, `RGWRados::Object::Read::prepare` (If-Modified-Since l.8433; If-None-Match l.8467–8476) — https://github.com/ceph/ceph/blob/ae5284f180e518926e74c36a2d91f6c2f594775d/src/rgw/driver/rados/rgw_rados.cc#L8433-L8476
- [rgw-common] Ceph `src/rgw/rgw_common.cc`, `rgw_string_unquote` l.532 — https://github.com/ceph/ceph/blob/ae5284f180e518926e74c36a2d91f6c2f594775d/src/rgw/rgw_common.cc#L532

Other primary sources:

- [ngx-core] nginx core module (`absolute_redirect`, `port_in_redirect`, `server_name_in_redirect`) — https://nginx.org/en/docs/http/ngx_http_core_module.html
- [ngx-proxy] nginx proxy module (`proxy_set_header`) — https://nginx.org/en/docs/http/ngx_http_proxy_module.html
- [ngx-headers] nginx headers module (`add_header … always`) — https://nginx.org/en/docs/http/ngx_http_headers_module.html
- [ngx-realip] nginx realip module — https://nginx.org/en/docs/http/ngx_http_realip_module.html
- [ngx-map] nginx map module — https://nginx.org/en/docs/http/ngx_http_map_module.html
- [ngx-gzip] nginx gzip module — https://nginx.org/en/docs/http/ngx_http_gzip_module.html
- [ngx-changes] nginx CHANGES, 1.7.3 ("strong ones are changed to weak") — https://nginx.org/en/CHANGES
- [RFC9110] HTTP Semantics §13.1.2 If-None-Match (weak comparison) — https://www.rfc-editor.org/rfc/rfc9110#section-13.1.2
- [RFC9111] HTTP Caching §5.2.2.6 no-transform — https://www.rfc-editor.org/rfc/rfc9111#section-5.2.2.6
- [systemd-exec] systemd.exec, `LoadCredential=` and `%d` — https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html
