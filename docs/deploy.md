# Deploying pub-hub

The runbook for the Portal on the RGW host. Everything runs under systemd, with no Docker. How the files in `deploy/` reach the host (by hand or through IaC) and TLS certificates stay outside pub-hub.

Steps marked **(owner, host)** are run by the owner on the RGW host. Everything else happens in this repo or on GitHub.

What ships in `deploy/`:

| File | Installed as |
|---|---|
| `pubhub-portal.service` | `/etc/systemd/system/pubhub-portal.service` |
| `portal.toml` | `/etc/pubhub/portal.toml` |
| `hub.bdgn.me.conf` | the nginx server block for `hub.bdgn.me`, wherever the host keeps them |
| `pub.bdgn.me.conf` | the nginx HTTP-context config for `pub.bdgn.me` (maps, rate zone and server) |

Releases are GitHub Releases, cut by pushing a `v*` tag. Each carries `pubhub-portal-linux-amd64`, `pubhub-portal-linux-arm64`, CLI binaries for Linux `amd64`/`arm64` and macOS `arm64`, and `checksums.txt`.

## Cut a release

```sh
git tag v0.7.0
git push origin v0.7.0
```

The Release workflow runs vet and tests, then publishes the binaries and checksums.

## Install

Complete the [RGW setup](#rgw-users-buckets-and-policies) and [Zitadel setup](#zitadel-machine-publisher-setup) before installing credentials or starting the Portal. The next steps use the users, buckets and application created there.

1. **(owner, host)** Create the `pubhub` system user and put nginx in its group, so nginx can reach the `0660` socket. The nginx user is `nginx`, `www-data` or `http` depending on the distribution.

   ```sh
   sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin pubhub
   sudo usermod -aG pubhub nginx
   ```

2. **(owner, host)** Create the config directory and install the config. Set the Zitadel issuer, `hub-api` client ID, `[publishers]` entries, RGW loopback endpoint, bucket names and `pub.` base URL in `/etc/pubhub/portal.toml`. The RGW endpoint must be the loopback URL, not `s3.bdgn.me`; secrets do not belong in this file.

   ```sh
   sudo install -d -m 0755 /etc/pubhub
   sudo install -m 0644 deploy/portal.toml /etc/pubhub/portal.toml
   sudoedit /etc/pubhub/portal.toml
   ```

3. **(owner, host)** Install the `hub-api` client secret and the `pub-hub` RGW access/secret keys as root-only systemd credentials. The unit exposes them to the Portal through `LoadCredential=`. Do not install the provisioning user's keys on the server.

   ```sh
   sudo install -d -o root -g root -m 0700 /etc/pubhub/credentials
   sudo install -o root -g root -m 0600 /path/to/hub-api-client-secret \
     /etc/pubhub/credentials/hub-api-client-secret
   sudo install -o root -g root -m 0600 /path/to/pub-hub-access-key-id \
     /etc/pubhub/credentials/rgw-access-key-id
   sudo install -o root -g root -m 0600 /path/to/pub-hub-secret-access-key \
     /etc/pubhub/credentials/rgw-secret-access-key
   ```

4. **(owner, host)** Install the binary from a release, as in [Download a release](#download-a-release), then:

   ```sh
   sudo install -m 0755 pubhub-portal-linux-$ARCH /usr/local/bin/pubhub-portal
   ```

5. **(owner, host)** Install and start the unit.

   ```sh
   sudo install -m 0644 deploy/pubhub-portal.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now pubhub-portal
   journalctl -u pubhub-portal -n 5   # expect {"msg":"portal listening","socket":"/run/pubhub/portal.sock",...}
   ```

6. **(owner, host)** Install the nginx server block. Set the certificate paths, and replace the `allow` lines with the host's LAN and VPN ranges, keeping `deny all` last.

   ```sh
   sudo nginx -t && sudo systemctl reload nginx
   ```

   nginx workers pick up the new `pubhub` group membership when they restart on reload. If `/healthz` answers `502` with `Permission denied` in the nginx error log, restart nginx instead.

7. **(owner)** Add an internal DNS record for `hub.bdgn.me` pointing at the host. There is no public record.

8. **(owner)** Verify from the LAN:

   ```sh
   curl -si https://hub.bdgn.me/healthz   # 200, with the hardening headers
   curl -si https://hub.bdgn.me/readyz    # 200; both storage checks are true
   curl -si -H "Authorization: Bearer $PAT" https://hub.bdgn.me/api/whoami
   ```

   The `whoami` request returns the configured Publisher label. From outside the LAN and VPN ranges, all three requests get `403`.

## Reader host on the LAN

**(owner, host)** After granting anonymous `GetObject` on the Artifact bucket, install `deploy/pub.bdgn.me.conf` in nginx's `http` context alongside the `hub.` config. It defines `map` and `limit_req_zone` directives outside the server block, so do not paste it inside a `server`. Set the existing wildcard TLS certificate and key paths in the file. Keep RGW at `127.0.0.1:7480` and the bucket name `pubhub-artifacts`, or change both in the file to match `portal.toml`. This server must not be exposed to the internet yet.

```sh
sudo nginx -t && sudo systemctl reload nginx
```

Add an **internal** DNS record for `pub.bdgn.me` pointing at this nginx host. From the LAN, publish a Bundle with the CLI and check the entry, its explicit index, the redirect and a missing key:

```sh
pubhub publish ./demo xform/demo --no-overwrite
curl -si https://pub.bdgn.me/xform/demo/
curl -si https://pub.bdgn.me/xform/demo/index.html
curl -si https://pub.bdgn.me/xform/demo    # 301, Location: /xform/demo/
curl -si https://pub.bdgn.me/xform/missing.html  # 404
curl -si https://pub.bdgn.me/robots.txt  # 404, noindex
```

Check `Content-Type`, `Cache-Control: no-cache, no-transform`, `X-Robots-Tag: noindex, nofollow` and `X-Content-Type-Options: nosniff` on both successful and error responses. nginx's access log records Reader IPs. Point the LAN uptime monitor at `https://hub.bdgn.me/readyz` and one known Artifact URL at `pub.`; checking the known URL also tests RGW serving, not just the Portal. The CI test runs the shipped config against local S3 and Artifacts published through the Portal. If RGW introduces new `x-amz-*` or `x-rgw-*` response headers, add their names to the nginx `proxy_hide_header` list and the integration check.

## RGW users, buckets and policies

**(owner, host)** Provision both buckets with the RGW S3 API. `pub-hub-owner` is for provisioning only; the Portal runs as `pub-hub` and receives only the access keys for that user.

1. Create the two users and allow `pub-hub` to use its credentials without granting it bucket-administration commands:

   ```sh
   sudo radosgw-admin user create --uid=pub-hub-owner --display-name='pub-hub bucket owner'
   sudo radosgw-admin user create --uid=pub-hub --display-name='pub-hub Portal'
   sudo radosgw-admin user modify --uid=pub-hub --max-buckets=-1
   ```

   Keep `pub-hub-owner`'s keys with the provisioning operator. Securely copy only `pub-hub`'s access key ID and secret access key into the credential files from Install step 3. Configure the provisioning keys in the owner's local AWS CLI profile named `pub-hub-owner`; do not copy that profile to the server.

2. Using the provisioning user's S3 credentials, create the buckets. Replace the endpoint port if the host's RGW listens elsewhere:

   ```sh
   RGW=http://127.0.0.1:7480
   AWS_PROFILE=pub-hub-owner
   export RGW AWS_PROFILE
   aws --endpoint-url "$RGW" --region default s3api create-bucket --bucket pubhub-artifacts
   aws --endpoint-url "$RGW" --region default s3api create-bucket --bucket pubhub-meta
   ```

3. Apply the bucket policies **before** enabling the metadata bucket's Public Access Block. These policies grant the Portal list/read/write/delete access and allow anonymous `GetObject` only on the Artifact bucket; they do not grant anonymous listing.

   ```sh
   cat >/tmp/pubhub-artifacts-policy.json <<'JSON'
   {"Version":"2012-10-17","Statement":[
     {"Sid":"PortalList","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/pub-hub"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::pubhub-artifacts"},
     {"Sid":"PortalObjects","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/pub-hub"},"Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::pubhub-artifacts/*"},
     {"Sid":"ReadersGetObjects","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pubhub-artifacts/*"}
   ]}
   JSON
   cat >/tmp/pubhub-meta-policy.json <<'JSON'
   {"Version":"2012-10-17","Statement":[
     {"Sid":"PortalList","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/pub-hub"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::pubhub-meta"},
     {"Sid":"PortalObjects","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/pub-hub"},"Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::pubhub-meta/*"}
   ]}
   JSON
   aws --endpoint-url "$RGW" --region default s3api put-bucket-policy \
     --bucket pubhub-artifacts --policy file:///tmp/pubhub-artifacts-policy.json
   aws --endpoint-url "$RGW" --region default s3api put-bucket-policy \
     --bucket pubhub-meta --policy file:///tmp/pubhub-meta-policy.json
   ```

4. Set Public Access Block only after the policy is in place. For Artifacts, only `IgnorePublicAcls` is needed; the bucket policy intentionally grants public reads. The private metadata bucket uses all protective settings except `BlockPublicAcls`:

   ```sh
   aws --endpoint-url "$RGW" --region default s3api put-public-access-block \
     --bucket pubhub-artifacts \
     --public-access-block-configuration '{"BlockPublicAcls":false,"IgnorePublicAcls":true,"BlockPublicPolicy":false,"RestrictPublicBuckets":false}'
   aws --endpoint-url "$RGW" --region default s3api put-public-access-block \
     --bucket pubhub-meta \
     --public-access-block-configuration '{"BlockPublicAcls":false,"IgnorePublicAcls":true,"BlockPublicPolicy":true,"RestrictPublicBuckets":true}'
   ```

   On Ceph Squid, never set `BlockPublicAcls: true`. RGW 19.2.0–19.2.4 then returns `403` for every `PutObject`. `BlockPublicPolicy: true` rejects policy changes, so lower that flag, change the policy, then restore it. Keep the metadata bucket policy private throughout.

5. After Install, publish a probe Artifact through the Portal and check the policy boundary from an unauthenticated machine. Anonymous `GetObject` for the known Artifact must succeed. The remaining requests must return `403` from RGW (the public nginx server later maps missing-object `403` to Reader-facing `404`):

   ```sh
   curl -f -X PUT -H "Authorization: Bearer $PAT" \
     -F "file=@plan.html" https://hub.bdgn.me/api/artifacts/xform/notes/plan.html
   curl -fsS "$RGW/pubhub-artifacts/xform/notes/plan.html" >/dev/null  # anonymous 200
   aws --no-sign-request --endpoint-url "$RGW" --region default s3api list-objects-v2 --bucket pubhub-artifacts
   aws --no-sign-request --endpoint-url "$RGW" --region default s3api get-object-acl \
     --bucket pubhub-artifacts --key xform/notes/plan.html
   aws --no-sign-request --endpoint-url "$RGW" --region default s3api put-object \
     --bucket pubhub-artifacts --key forbidden.html --body /etc/hostname
   aws --no-sign-request --endpoint-url "$RGW" --region default s3api delete-object \
     --bucket pubhub-artifacts --key xform/notes/plan.html
   aws --no-sign-request --endpoint-url "$RGW" --region default s3api get-object \
     --bucket pubhub-artifacts --key does-not-exist.html /tmp/missing.html
   ```

   Each of the final five commands should fail with `403`; only the first public read succeeds. For `get-object` use a key known not to exist. Do not add a public `ListBucket` grant to make the missing-key response a `404` at RGW.

To check Bundle replacement on the host after upgrading, publish a local directory
with `index.html` and at least one other file, then remove that other file and
publish again:

```sh
curl -f -X PUT -H "Authorization: Bearer $PAT" \
  -F 'index.html=@demo/index.html' -F 'assets/app.css=@demo/assets/app.css' \
  https://hub.bdgn.me/api/artifacts/xform/demo/
rm demo/assets/app.css
curl -f -X PUT -H "Authorization: Bearer $PAT" \
  -F 'index.html=@demo/index.html' https://hub.bdgn.me/api/artifacts/xform/demo/
curl -fsS https://pub.bdgn.me/xform/demo/ >/dev/null  # 200
# The removed asset must return 404 through the Reader host.
curl -si https://pub.bdgn.me/xform/demo/assets/app.css
```

The Portal clears `/var/cache/pubhub/spool` at startup and after each request.
The `/api/` nginx location permits 101 MB bodies and waits up to 900 seconds
for a synchronous publish.

## Zitadel machine-Publisher setup

**(owner)** Do these steps in Zitadel before starting the Portal:

1. Create the `pub-hub` project and the `hub-api` confidential application for the Portal's token-introspection requests. Record its client ID and client secret. Use the canonical Zitadel hostname for the issuer URL.
2. Create one service account for each agent host and for the owner's CLI. Give each account a PAT with a one-year expiry, and record the account's user ID.
3. Put each user ID in the `[publishers]` table in `portal.toml`, mapping it to the label that should appear as the Publisher. Restart the Portal after changing the allowlist.
4. Store the `hub-api` client secret in `/etc/pubhub/credentials/hub-api-client-secret` as described above. The secret is never committed or placed in `portal.toml`.

The Portal sends `Authorization: Bearer <PAT>` to `/api/` and introspects the PAT through `hub-api`; agents do not need access to Zitadel. A missing or inactive PAT gets `401`, an active user absent from `[publishers]` gets `403`, and a Zitadel outage gets retryable `503`.

## Download a release

**(owner, host)** Pick the version and the host's architecture (`amd64` or `arm64`), then download and verify:

```sh
VERSION=v0.7.0
ARCH=amd64
BASE=https://github.com/yet-an-other/pub-hub/releases/download/$VERSION
curl -fLO "$BASE/pubhub-portal-linux-$ARCH"
curl -fLO "$BASE/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
```

## Upgrade

**(owner, host)** Download the new release as above, keep the running binary for rollback, install and restart:

```sh
sudo cp /usr/local/bin/pubhub-portal /usr/local/bin/pubhub-portal.prev
sudo install -m 0755 pubhub-portal-linux-$ARCH /usr/local/bin/pubhub-portal
sudo systemctl restart pubhub-portal
curl -si https://hub.bdgn.me/healthz
```

If the release notes change `portal.toml`, the unit or the nginx server block, install those from the release's tag before restarting. The Portal refuses to start on an unknown or invalid config key and logs why:

```sh
journalctl -u pubhub-portal -n 20
```

## Install or upgrade the CLI

Run `scripts/install-pubhub.sh` from a checkout as the user who will publish, without `sudo`. It detects Linux `x86_64`/`aarch64` or M-series macOS, downloads the CLI for the latest non-prerelease GitHub release, verifies its SHA-256 against the release's `checksums.txt`, and installs it at `~/.local/bin/pubhub`. It skips replacing an identical binary. `curl`, `awk`, `grep`, `install`, and either `sha256sum` or `shasum` are required. Ensure `~/.local/bin` is on your `PATH`.

```sh
sh scripts/install-pubhub.sh          # latest release
sh scripts/install-pubhub.sh v0.6.0   # pin or roll back to an available release
```

On first install, when there is no `$XDG_CONFIG_HOME/pubhub/config.toml` (or `~/.config/pubhub/config.toml`), the script runs `pubhub login`. Enter the PAT from the owner's Zitadel service account; the CLI validates it through the Portal and saves it in a private `0600` config file. The account must be on the Portal's `[publishers]` allowlist. Run the installer with an interactive terminal for a hidden PAT prompt. If login fails, the verified CLI stays installed so you can retry with `~/.local/bin/pubhub login`.

Subsequent runs leave credentials unchanged, even if the PAT has expired. Renew one with `pubhub login`. `PUBHUB_URL` sets the Portal URL during login; the default is `https://hub.bdgn.me`. A pinned release must include a CLI binary for your platform: `v0.5.0` has only Linux builds, so macOS needs a later release. This script installs the CLI only; upgrading the Portal still follows [Upgrade](#upgrade).

## Rollback

**(owner, host)** Put the previous binary back and restart:

```sh
sudo install -m 0755 /usr/local/bin/pubhub-portal.prev /usr/local/bin/pubhub-portal
sudo systemctl restart pubhub-portal
curl -si https://hub.bdgn.me/healthz
```

To roll back further than one release, download that version as in [Download a release](#download-a-release) and install it the same way. If the upgrade also changed `portal.toml`, the unit or the nginx server block, restore those from the older tag too.
