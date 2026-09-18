#!/bin/sh
# Install a Continuum release: download the archive for this OS/arch, verify it
# against the release's SHA256SUMS, and place `continuum` on PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/NakliTechie/continuum/main/scripts/install.sh | sh
#
# Environment: CONTINUUM_VERSION (default: the pinned release below),
#              CONTINUUM_INSTALL_DIR (default: $HOME/.local/bin).
set -eu

VERSION="${CONTINUUM_VERSION:-0.1.0-alpha.2}"
DIR="${CONTINUUM_INSTALL_DIR:-$HOME/.local/bin}"
BASE="https://github.com/NakliTechie/continuum/releases/download/v${VERSION}"

fail() { echo "install.sh: $*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in darwin|linux) ;; *) fail "unsupported OS: $os (Continuum needs a Unix PTY; on Windows use WSL2)";; esac
arch=$(uname -m)
case "$arch" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) fail "unsupported architecture: $arch" ;;
esac

pkg="continuum_${VERSION}_${os}_${arch}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"

echo "downloading $pkg.tar.gz"
curl -fsSL -o "$pkg.tar.gz" "$BASE/$pkg.tar.gz"
curl -fsSL -o SHA256SUMS "$BASE/SHA256SUMS"
grep " $pkg.tar.gz\$" SHA256SUMS > expected || fail "SHA256SUMS has no entry for $pkg.tar.gz"
if command -v sha256sum >/dev/null 2>&1; then sha256sum -c expected >/dev/null
else shasum -a 256 -c expected >/dev/null
fi || fail "checksum mismatch for $pkg.tar.gz"

tar -xzf "$pkg.tar.gz"
mkdir -p "$DIR"
install -m 0755 "$pkg/continuum" "$DIR/continuum"
echo "installed $("$DIR/continuum" version) to $DIR/continuum"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) echo "note: $DIR is not on your PATH" ;;
esac
