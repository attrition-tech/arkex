#!/bin/sh
# arkex installer - POSIX sh, needs only curl/wget and tar.
#
#   curl -fsSL https://get.arkex.dev/install.sh | sh
#
# Options (pass after `sh -s --`, e.g. `| sh -s -- --version 0.1.0`):
#   --version X.Y.Z      install a specific release (default: latest stable)
#   --channel NAME       release channel to follow (default: stable)
#   --dir DIR            install directory (default: $ARKEX_HOME/bin or ~/.arkex/bin)
#   --no-modify-path     do not add the install dir to your shell rc files
#   --uninstall          remove the binary and PATH lines this script added
#
# Environment: ARKEX_HOME, ARKEX_INSTALL_DIR, ARKEX_VERSION,
#   ARKEX_DOWNLOAD_BASE (release host; default https://get.arkex.dev).
#
# Later, update in place with:  arkex update
set -eu

BASE="${ARKEX_DOWNLOAD_BASE:-https://get.arkex.dev}"
BIN="arkex"
VERSION="${ARKEX_VERSION:-latest}"
CHANNEL="stable"
# ARKEX_REQUIRE_SIGNATURE=1 refuses to install when the ed25519 signature
# cannot be checked (older openssl/LibreSSL) instead of trusting TLS + checksum.
REQUIRE_SIG="${ARKEX_REQUIRE_SIGNATURE:-}"
INSTALL_DIR="${ARKEX_INSTALL_DIR:-}"
MODIFY_PATH=1
UNINSTALL=0

# ed25519 release key (SubjectPublicKeyInfo). Must match internal/update/keys.txt.
PUBKEY_PEM='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA++8T50Iox71q3eRkAt6DJkQ6+O4yJg74FzsanJqbSyc=
-----END PUBLIC KEY-----'

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#*=}"; shift ;;
    --channel) CHANNEL="$2"; shift 2 ;;
    --channel=*) CHANNEL="${1#*=}"; shift ;;
    --dir) INSTALL_DIR="$2"; shift 2 ;;
    --dir=*) INSTALL_DIR="${1#*=}"; shift ;;
    --no-modify-path) MODIFY_PATH=0; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$INSTALL_DIR" ]; then
  INSTALL_DIR="${ARKEX_HOME:-$HOME/.arkex}/bin"
fi

say()  { printf '%s\n' "$*" >&2; }
fail() { say "error: $*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

fetch() { # fetch URL [OUT]
  if have curl; then
    if [ -n "${2:-}" ]; then curl -fsSL -o "$2" "$1"; else curl -fsSL "$1"; fi
  elif have wget; then
    if [ -n "${2:-}" ]; then wget -q -O "$2" "$1"; else wget -qO- "$1"; fi
  else
    fail "need curl or wget"
  fi
}

rc_files() {
  for f in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.zshrc"; do
    [ -f "$f" ] && printf '%s\n' "$f"
  done
  [ -n "${XDG_CONFIG_HOME:-}" ] && [ -f "$XDG_CONFIG_HOME/fish/config.fish" ] && printf '%s\n' "$XDG_CONFIG_HOME/fish/config.fish"
  [ -f "$HOME/.config/fish/config.fish" ] && printf '%s\n' "$HOME/.config/fish/config.fish"
  return 0
}

MARK="# added by arkex installer"

if [ "$UNINSTALL" = 1 ]; then
  rm -f "$INSTALL_DIR/$BIN"
  for f in $(rc_files); do
    if grep -q "$MARK" "$f" 2>/dev/null; then
      tmp="$f.arkex.tmp"
      grep -v "$MARK" "$f" > "$tmp" && mv "$tmp" "$f"
    fi
  done
  say "removed $INSTALL_DIR/$BIN (config in ${ARKEX_HOME:-$HOME/.arkex} left in place)"
  exit 0
fi

# --- detect platform --------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  mingw*|msys*|cygwin*) fail "Windows: download the zip from $BASE (install.ps1 coming)" ;;
  *) fail "unsupported OS: $os" ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail "unsupported architecture: $arch" ;;
esac

# Downloads must be HTTPS; plain http is only for loopback test mirrors.
case "$BASE" in
  https://*) ;;
  http://localhost*|http://127.0.0.1*|http://\[::1\]*) ;;
  *) fail "ARKEX_DOWNLOAD_BASE must be an https:// URL (got $BASE)" ;;
esac

# --- resolve version --------------------------------------------------------
if [ "$VERSION" = latest ]; then
  VERSION=$(fetch "$BASE/$CHANNEL.json" | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' | head -n1)
  [ -n "$VERSION" ] || fail "could not read $BASE/$CHANNEL.json (is the release host up?)"
fi
plain="${VERSION#v}"
tag="v$plain"

archive="${BIN}_${plain}_${os}_${arch}.tar.gz"
dir="$BASE/$tag"

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t arkex)
trap 'rm -rf "$tmp"' EXIT

say "downloading arkex ${tag} (${os}/${arch}) from ${BASE} ..."
fetch "$dir/$archive" "$tmp/$archive"
fetch "$dir/SHA256SUMS" "$tmp/SHA256SUMS"
fetch "$dir/SHA256SUMS.sig" "$tmp/SHA256SUMS.sig"

# --- verify -----------------------------------------------------------------
# 1. ed25519 signature over SHA256SUMS (when openssl >= 1.1.1 / LibreSSL >= 3.7 is
#    available; `arkex update` always verifies it in-process).
if have openssl; then
  printf '%s\n' "$PUBKEY_PEM" > "$tmp/release.pub"
  # .sig line: "<keyid> <base64 signature>"
  awk 'NF>=2 && $1 !~ /^#/ {print $2; exit}' "$tmp/SHA256SUMS.sig" | openssl base64 -d -A > "$tmp/sig.bin" 2>/dev/null || true
  result=$(openssl pkeyutl -verify -pubin -inkey "$tmp/release.pub" -rawin -in "$tmp/SHA256SUMS" -sigfile "$tmp/sig.bin" 2>&1 || true)
  case "$result" in
    *"Verified Successfully"*) verified=1; say "signature verified (ed25519)" ;;
    *"Verification Failure"*)  fail "SHA256SUMS signature does not verify - refusing to install" ;;
    *) say "note: this openssl cannot verify ed25519; trusting TLS to $BASE plus the checksum" ;;
  esac
else
  say "note: openssl not found; trusting TLS to $BASE plus the checksum"
fi
if [ -n "$REQUIRE_SIG" ] && [ -z "${verified:-}" ]; then
  fail "ARKEX_REQUIRE_SIGNATURE is set and the signature could not be verified (need openssl >= 1.1.1 or LibreSSL >= 3.7)"
fi

# 2. archive checksum pinned by SHA256SUMS.
expected=$(grep " $archive\$" "$tmp/SHA256SUMS" | awk '{print $1}')
[ -n "$expected" ] || fail "no checksum for $archive in SHA256SUMS (no build for $os/$arch?)"
if have sha256sum; then actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
elif have shasum; then actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
elif have openssl; then actual=$(openssl dgst -sha256 "$tmp/$archive" | awk '{print $NF}')
else fail "need sha256sum, shasum or openssl to verify the download"; fi
[ "$expected" = "$actual" ] || fail "checksum mismatch for $archive
expected $expected
got      $actual"

# --- install ----------------------------------------------------------------
# The archive is expected to hold a handful of regular files at the top level.
# Refuse anything that could write elsewhere: paths with a slash or "..",
# absolute paths, links or devices (type letter other than "-").
tar -tzvf "$tmp/$archive" > "$tmp/members" || fail "could not list $archive"
if awk '{ type = substr($1, 1, 1); name = $NF }
        type != "-" || name ~ /^\// || name ~ /\// || name ~ /\.\./ { bad = 1 }
        END { exit bad }' "$tmp/members"; then :; else
  fail "$archive has unexpected members - refusing to extract:
$(cat "$tmp/members")"
fi
grep -q " $BIN\$" "$tmp/members" || fail "$archive does not contain $BIN"
tar -xzf "$tmp/$archive" -C "$tmp" "$BIN"
mkdir -p "$INSTALL_DIR"
# Stage beside the target so the final mv is an atomic same-filesystem rename.
cp "$tmp/$BIN" "$INSTALL_DIR/.$BIN.new"
chmod 755 "$INSTALL_DIR/.$BIN.new"
mv -f "$INSTALL_DIR/.$BIN.new" "$INSTALL_DIR/$BIN"
say "installed $INSTALL_DIR/$BIN ($("$INSTALL_DIR/$BIN" --version 2>/dev/null || echo "$tag"))"

# --- PATH -------------------------------------------------------------------
case ":$PATH:" in
  *":$INSTALL_DIR:"*) on_path=1 ;;
  *) on_path=0 ;;
esac
if [ "$on_path" = 0 ] && [ "$MODIFY_PATH" = 1 ]; then
  added=0
  for f in $(rc_files); do
    grep -q "$MARK" "$f" 2>/dev/null && continue
    case "$f" in
      *fish*) printf '\nfish_add_path %s %s\n' "$INSTALL_DIR" "$MARK" >> "$f" ;;
      *) printf '\nexport PATH="%s:$PATH" %s\n' "$INSTALL_DIR" "$MARK" >> "$f" ;;
    esac
    added=1
  done
  [ "$added" = 1 ] && say "added $INSTALL_DIR to PATH in your shell rc."
  say "this shell does not see it yet; run one of:"
  say "  exec \$SHELL -l                         # reload this tab"
  say "  export PATH=\"$INSTALL_DIR:\$PATH\"   # just this tab"
elif [ "$on_path" = 0 ]; then
  say "add to PATH: export PATH=\"$INSTALL_DIR:\$PATH\""
fi

say ""
say "next: arkex          # opens the TUI; press a to add your first model server"
say "      arkex update   # later, to pick up new releases (no shell restart needed)"
