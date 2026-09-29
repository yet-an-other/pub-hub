#!/bin/sh
# Install or upgrade the pubhub CLI for the current user. Do not run as root.
set -eu

usage() {
    echo "Usage: $0 [vMAJOR.MINOR.PATCH]" >&2
    exit 2
}

[ "$#" -le 1 ] || usage
if [ "$(id -u)" -eq 0 ]; then
    echo "Run this as the user who will publish, not as root or with sudo." >&2
    exit 1
fi

case "$(uname -s):$(uname -m)" in
    Linux:x86_64|Linux:amd64) platform=linux-amd64 ;;
    Linux:aarch64|Linux:arm64) platform=linux-arm64 ;;
    Darwin:arm64) platform=darwin-arm64 ;;
    *) echo "Unsupported OS/architecture: $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac

command -v curl >/dev/null 2>&1 || { echo 'curl is required' >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
    hash_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
    hash_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
    echo 'sha256sum or shasum is required' >&2
    exit 1
fi

repo=https://github.com/yet-an-other/pub-hub
if [ "$#" -eq 1 ]; then
    version=$1
else
    # GitHub redirects /releases/latest to the latest non-prerelease tag.
    latest=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
    version=${latest##*/}
fi
if ! printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "Invalid or unavailable release version: $version" >&2
    exit 1
fi

asset=pubhub-$platform
base=$repo/releases/download/$version
work=$(mktemp -d)
stage=
cleanup() {
    [ -z "$stage" ] || rm -f "$stage"
    rm -rf "$work"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

curl -fsSL --retry 3 -o "$work/$asset" "$base/$asset"
curl -fsSL --retry 3 -o "$work/checksums.txt" "$base/checksums.txt"
# Match the filename exactly, not a substring such as pubhub-linux-arm64-extra.
expected=$(awk -v name="$asset" '$2 == name { count++; checksum = $1 } END { if (count == 1) print checksum }' "$work/checksums.txt")
if ! printf '%s\n' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$'; then
    echo "Missing or invalid checksum for $asset in $version" >&2
    exit 1
fi
actual=$(hash_file "$work/$asset")
if [ "$actual" != "$expected" ]; then
    echo "Checksum mismatch for $asset in $version; existing CLI untouched" >&2
    exit 1
fi

bin_dir=$HOME/.local/bin
binary=$bin_dir/pubhub
if [ -f "$binary" ] && [ "$(hash_file "$binary")" = "$expected" ]; then
    echo "pubhub $version already installed at $binary"
else
    mkdir -p "$bin_dir"
    stage=$(mktemp "$bin_dir/.pubhub.XXXXXXXX")
    install -m 0755 "$work/$asset" "$stage"
    mv -f "$stage" "$binary"
    stage=
    echo "Installed pubhub $version at $binary"
fi

config_dir=${XDG_CONFIG_HOME:-$HOME/.config}
if [ ! -f "$config_dir/pubhub/config.toml" ]; then
    echo 'No pubhub credentials found. Enter a Zitadel PAT to log in.'
    # pubhub login validates the PAT and writes config.toml with mode 0600.
    if ! "$binary" login; then
        echo "Login failed; pubhub remains installed. Retry with: $binary login" >&2
        exit 1
    fi
fi

case ":$PATH:" in
    *":$bin_dir:"*) ;;
    *) echo "Add $bin_dir to your PATH to run pubhub from any directory." >&2 ;;
esac
