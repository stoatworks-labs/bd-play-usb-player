#!/usr/bin/env bash
# Cross-compile bdplay for the BirdDog PLAY (aarch64 Linux, Debian 10).
#
# Unlike bdkvm and bdcam this needs no cgo and therefore no zig: bdplay never
# dlopens anything, it drives gst-launch-1.0 and mount as subprocesses. So a
# plain CGO_ENABLED=0 build produces a fully static binary that runs on the
# device's 2019-vintage glibc without any toolchain pinning.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

command -v go >/dev/null || { echo "error: go not found" >&2; exit 1; }

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
  -o dist/bdplay-linux-arm64 .

echo "built:   dist/bdplay-linux-arm64"
echo "version: ${VERSION}"
file dist/bdplay-linux-arm64
echo "size:    $(du -h dist/bdplay-linux-arm64 | cut -f1)"
