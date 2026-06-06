#!/bin/sh
# olf installer. Usage:
#   curl -sSf https://raw.githubusercontent.com/datumin/open-lifelog/main/install.sh | sh
# Env: OLF_VERSION=vX.Y.Z  OLF_INSTALL_DIR=/path  OLF_REQUIRE_COSIGN=1
set -eu

REPO="datumin/open-lifelog"
INSTALL_DIR="${OLF_INSTALL_DIR:-/usr/local/bin}"

err() { echo "olf-install: $*" >&2; exit 1; }
info() { echo "olf-install: $*" >&2; }

need() { command -v "$1" >/dev/null 2>&1 || err "required command not found: $1"; }
need curl
need tar
need uname

# --- OS/arch detection ---
os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (Windows: download the zip from Releases, or use 'go install open-lifelog.org/node/cmd/olf@latest')" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) err "unsupported arch: $arch" ;;
esac

# --- version resolution ---
ver="${OLF_VERSION:-}"
if [ -z "$ver" ]; then
  ver=$(curl -sSf "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -m1 '"tag_name"' | cut -d'"' -f4)
  [ -n "$ver" ] || err "could not resolve latest version"
fi
num=${ver#v}   # strip leading v for archive name
info "installing olf $ver for ${os}/${arch}"

# --- download ---
base="https://github.com/$REPO/releases/download/$ver"
archive="olf_${num}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -sSfL "$base/$archive" -o "$tmp/$archive" || err "download failed: $archive"
curl -sSfL "$base/checksums.txt" -o "$tmp/checksums.txt" || err "download failed: checksums.txt"

# --- checksum verification (mandatory) ---
sumcmd=""
if command -v sha256sum >/dev/null 2>&1; then sumcmd="sha256sum"
elif command -v shasum >/dev/null 2>&1; then sumcmd="shasum -a 256"
else err "no sha256 tool found (sha256sum/shasum)"; fi

want=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || err "checksum for $archive not found in checksums.txt"
# shellcheck disable=SC2086 # $sumcmd may be "shasum -a 256"; must word-split into args
got=$( (cd "$tmp" && $sumcmd "$archive") | awk '{print $1}')
[ "$want" = "$got" ] || err "checksum mismatch for $archive"
info "checksum OK"

# --- cosign verification (best-effort) ---
if command -v cosign >/dev/null 2>&1; then
  curl -sSfL "$base/checksums.txt.sig" -o "$tmp/checksums.txt.sig" || err "download failed: checksums.txt.sig"
  curl -sSfL "$base/checksums.txt.pem" -o "$tmp/checksums.txt.pem" || err "download failed: checksums.txt.pem"
  cosign verify-blob \
    --certificate "$tmp/checksums.txt.pem" \
    --signature "$tmp/checksums.txt.sig" \
    --certificate-identity-regexp "https://github.com/$REPO" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$tmp/checksums.txt" >/dev/null 2>&1 || err "cosign signature verification failed"
  info "cosign signature OK"
else
  if [ "${OLF_REQUIRE_COSIGN:-0}" = "1" ]; then
    err "cosign not found but OLF_REQUIRE_COSIGN=1"
  fi
  info "cosign not found; skipping signature check (set OLF_REQUIRE_COSIGN=1 to require it)"
fi

# --- install ---
tar -xzf "$tmp/$archive" -C "$tmp" olf || err "extract failed"

dest="$INSTALL_DIR/olf"
if [ -w "$INSTALL_DIR" ] 2>/dev/null; then
  install -m 0755 "$tmp/olf" "$dest"
elif command -v sudo >/dev/null 2>&1; then
  info "installing to $INSTALL_DIR via sudo"
  sudo install -m 0755 "$tmp/olf" "$dest"
else
  dest="$HOME/.local/bin/olf"
  mkdir -p "$HOME/.local/bin"
  install -m 0755 "$tmp/olf" "$dest"
  info "no write access to $INSTALL_DIR; installed to $dest (ensure ~/.local/bin is on PATH)"
fi

info "installed: $("$dest" version) -> $dest"
