# Deploying pub-hub

The runbook for the Portal on the RGW host. Everything runs under systemd, with no Docker. How the files in `deploy/` reach the host (by hand or through IaC) and TLS certificates stay outside pub-hub.

Steps marked **(owner, host)** are run by the owner on the RGW host. Everything else happens in this repo or on GitHub.

What ships in `deploy/`:

| File | Installed as |
|---|---|
| `pubhub-portal.service` | `/etc/systemd/system/pubhub-portal.service` |
| `portal.toml` | `/etc/pubhub/portal.toml` |
| `hub.bdgn.me.conf` | the nginx server block for `hub.bdgn.me`, wherever the host keeps them |

Releases are GitHub Releases, cut by pushing a `v*` tag. Each carries `pubhub-portal-linux-amd64`, `pubhub-portal-linux-arm64` and `checksums.txt`.

## Cut a release

```sh
git tag v0.1.0
git push origin v0.1.0
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
VERSION=v0.1.0
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

## Rollback

**(owner, host)** Put the previous binary back and restart:

```sh
sudo install -m 0755 /usr/local/bin/pubhub-portal.prev /usr/local/bin/pubhub-portal
sudo systemctl restart pubhub-portal
curl -si https://hub.bdgn.me/healthz
```

To roll back further than one release, download that version as in [Download a release](#download-a-release) and install it the same way. If the upgrade also changed `portal.toml`, the unit or the nginx server block, restore those from the older tag too.
