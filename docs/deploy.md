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

1. **(owner, host)** Create the `pubhub` system user and put nginx in its group, so nginx can reach the `0660` socket. The nginx user is `nginx`, `www-data` or `http` depending on the distribution.

   ```sh
   sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin pubhub
   sudo usermod -aG pubhub nginx
   ```

2. **(owner, host)** Create the config directory and install the config. Adjust nothing yet: the example is the working config for this stage.

   ```sh
   sudo install -d -m 0755 /etc/pubhub
   sudo install -m 0644 deploy/portal.toml /etc/pubhub/portal.toml
   ```

3. **(owner, host)** Install the binary from a release, as in [Download a release](#download-a-release), then:

   ```sh
   sudo install -m 0755 pubhub-portal-linux-$ARCH /usr/local/bin/pubhub-portal
   ```

4. **(owner, host)** Install and start the unit.

   ```sh
   sudo install -m 0644 deploy/pubhub-portal.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now pubhub-portal
   journalctl -u pubhub-portal -n 5   # expect {"msg":"portal listening","socket":"/run/pubhub/portal.sock",...}
   ```

5. **(owner, host)** Install the nginx server block. Set the certificate paths, and replace the `allow` lines with the host's LAN and VPN ranges, keeping `deny all` last.

   ```sh
   sudo nginx -t && sudo systemctl reload nginx
   ```

   nginx workers pick up the new `pubhub` group membership when they restart on reload. If `/healthz` answers `502` with `Permission denied` in the nginx error log, restart nginx instead.

6. **(owner)** Add an internal DNS record for `hub.bdgn.me` pointing at the host. There is no public record.

7. **(owner)** Verify from the LAN:

   ```sh
   curl -si https://hub.bdgn.me/healthz   # 200, with the hardening headers
   ```

   From outside the LAN and VPN ranges, the same request gets `403`.

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
