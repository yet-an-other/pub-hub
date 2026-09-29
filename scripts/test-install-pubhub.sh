#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
repo=$(pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' 0
mkdir -p "$tmp/mock" "$tmp/home" "$tmp/assets"
export TEST_HOME="$tmp/home" TEST_ASSETS="$tmp/assets" TEST_LOG="$tmp/log" HOME="$tmp/home"
export PATH="$tmp/mock:$PATH"
export XDG_CONFIG_HOME="$tmp/home/config"

# The release fixture is a fake CLI, so the test needs no live Portal or PAT.
for platform in linux-amd64 darwin-arm64; do
    printf '#!/bin/sh\n[ "$1" = login ] || exit 1\necho login >> "$TEST_LOG"\n[ "${TEST_LOGIN_FAIL:-0}" = 0 ] || exit 1\nmkdir -p "$XDG_CONFIG_HOME/pubhub"\nprintf "token = \\"test\\"\\n" > "$XDG_CONFIG_HOME/pubhub/config.toml"\nchmod 600 "$XDG_CONFIG_HOME/pubhub/config.toml"\n' > "$tmp/assets/pubhub-$platform"
done
(cd "$tmp/assets" && sha256sum pubhub-* > checksums.txt)

printf '#!/bin/sh\nif [ "$1" = -u ]; then echo 1000; else /usr/bin/id "$@"; fi\n' > "$tmp/mock/id"
printf '#!/bin/sh\nif [ "$1" = -s ]; then echo "${TEST_OS:-Linux}"; else echo "${TEST_ARCH:-x86_64}"; fi\n' > "$tmp/mock/uname"
cat > "$tmp/mock/curl" <<'EOF'
#!/bin/sh
out=
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        https://*) url=$1 ;;
        -o) shift; out=$1 ;;
    esac
    shift
done
case "$url" in
    */releases/latest) printf '%s\n' 'https://github.com/yet-an-other/pub-hub/releases/tag/v0.6.0' ;;
    */checksums.txt) cp "$TEST_ASSETS/checksums.txt" "$out" ;;
    */pubhub-*) cp "$TEST_ASSETS/${url##*/}" "$out" ;;
    *) echo "Unexpected URL: $url" >&2; exit 1 ;;
esac
EOF
chmod +x "$tmp/mock/"*

sh "$repo/scripts/install-pubhub.sh" > "$tmp/output"
[ -f "$HOME/.local/bin/pubhub" ]
[ "$(wc -l < "$TEST_LOG")" -eq 1 ]
# A second run skips the replacement and never prompts again.
sh "$repo/scripts/install-pubhub.sh" > "$tmp/output"
grep -q 'already installed' "$tmp/output"
[ "$(wc -l < "$TEST_LOG")" -eq 1 ]
# A mismatch must leave the installed binary untouched.
cp "$HOME/.local/bin/pubhub" "$tmp/saved"
printf 'corrupt\n' > "$tmp/assets/pubhub-linux-amd64"
if sh "$repo/scripts/install-pubhub.sh" v0.6.0 > "$tmp/output" 2>&1; then
    echo 'Checksum mismatch was accepted' >&2; exit 1
fi
cmp "$tmp/saved" "$HOME/.local/bin/pubhub"
# An unsupported platform must fail before downloading.
if TEST_OS=Darwin TEST_ARCH=x86_64 sh "$repo/scripts/install-pubhub.sh" > "$tmp/output" 2>&1; then
    echo 'Unsupported platform was accepted' >&2; exit 1
fi
# A macOS M-series run selects its own release asset.
TEST_OS=Darwin TEST_ARCH=arm64 sh "$repo/scripts/install-pubhub.sh" v0.6.0 > "$tmp/output"
cmp "$tmp/assets/pubhub-darwin-arm64" "$HOME/.local/bin/pubhub"
[ "$(wc -l < "$TEST_LOG")" -eq 1 ]
rm "$XDG_CONFIG_HOME/pubhub/config.toml"
if TEST_LOGIN_FAIL=1 TEST_OS=Darwin TEST_ARCH=arm64 sh "$repo/scripts/install-pubhub.sh" v0.6.0 > "$tmp/output" 2>&1; then
    echo 'Failed login was accepted' >&2; exit 1
fi
cmp "$tmp/assets/pubhub-darwin-arm64" "$HOME/.local/bin/pubhub"
[ ! -e "$XDG_CONFIG_HOME/pubhub/config.toml" ]
echo 'installer tests passed'
