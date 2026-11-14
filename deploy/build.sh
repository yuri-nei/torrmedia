#!/bin/bash
# Builds torrmedia for one platform and refreshes SHA256SUMS next to it.
#
#   deploy/build.sh linux [amd64|arm64]   -> deploy/linux/torrmedia-<version>
#   deploy/build.sh macos [arm64|amd64]   -> deploy/macos/torrmedia-<version>
#
# <version> is the version constant in main.go. Linux defaults to amd64, macOS
# to the architecture of the machine running the build. The install script in
# the same directory installs the newest version listed in SHA256SUMS.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

case "${1:-}" in
  linux) GOOS=linux; ARCH="${2:-amd64}" ;;
  macos) GOOS=darwin; ARCH="${2:-$(uname -m | sed 's/^x86_64$/amd64/')}" ;;
  *) echo "usage: $0 linux|macos [amd64|arm64]" >&2; exit 2 ;;
esac
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 2 ;;
esac

VERSION="$(sed -n 's/^const version = "\(.*\)"$/\1/p' "$ROOT/main.go")"
[ -n "$VERSION" ] || { echo "version constant not found in main.go" >&2; exit 1; }

OUT="$ROOT/deploy/$1"
( cd "$ROOT" && CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$ARCH" go build -trimpath -o "$OUT/torrmedia-$VERSION" . )
(
  cd "$OUT"
  if command -v sha256sum >/dev/null; then sha256sum torrmedia-*; else shasum -a 256 torrmedia-*; fi > SHA256SUMS
)
echo "built $OUT/torrmedia-$VERSION ($GOOS/$ARCH)"
