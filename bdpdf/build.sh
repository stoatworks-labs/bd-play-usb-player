#!/usr/bin/env bash
# Cross-compile bdpdf (PDF -> PNG) for the BirdDog PLAY, against PDFium.
#
# Unlike tools/build-mutool.sh and tools/build-exfat.sh, this one IS safe to
# ship from the public web patcher:
#
#   * PDFium is BSD-3-Clause (Google/Foxit) — permissive, no copyleft, no §13.
#   * bblanchon/pdfium-binaries publishes a prebuilt linux-arm64 libpdfium.so
#     that needs only GLIBC_2.17 (the device has 2.28) and no libstdc++ — it
#     links its own C++ runtime statically. Its only NEEDED entries are
#     libpthread, libm, libgcc_s, libc and the loader, all present on stock
#     firmware. Verified on hardware.
#   * ~8 MB rather than MuPDF's 37 MB, so it fits Cloudflare's 25 MiB per-file
#     static asset limit with room to spare.
#
# Deliberately the GNU build, not the musl one: the musl .so has NEEDED libc.so
# and would want musl's loader, which a Debian 10 device does not have.
set -euo pipefail

PDFIUM_TAG="${PDFIUM_TAG:-latest}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="${WORK:-$HERE/../build/pdfium}"
DIST="$HERE/../dist"

command -v zig >/dev/null || { echo "error: zig not found (brew install zig)" >&2; exit 1; }
command -v gh  >/dev/null || { echo "error: gh not found (brew install gh)" >&2; exit 1; }

SDK="$WORK/gnu-arm64"
mkdir -p "$WORK"

if [ ! -f "$SDK/lib/libpdfium.so" ]; then
  echo "fetching PDFium ($PDFIUM_TAG) for linux-arm64"
  ( cd "$WORK"
    if [ "$PDFIUM_TAG" = latest ]; then
      gh release download --repo bblanchon/pdfium-binaries \
        --pattern "pdfium-linux-arm64.tgz" --clobber
    else
      gh release download "$PDFIUM_TAG" --repo bblanchon/pdfium-binaries \
        --pattern "pdfium-linux-arm64.tgz" --clobber
    fi
    mkdir -p gnu-arm64
    tar xzf pdfium-linux-arm64.tgz -C gnu-arm64 )
fi

[ -f "$SDK/include/fpdfview.h" ] || { echo "error: PDFium headers missing in $SDK" >&2; exit 1; }

mkdir -p "$DIST"

# glibc 2.28 pinned to match Debian 10 on the device. This links dynamically —
# a fully static glibc link is impossible (zig refuses), and pointless here
# because libpdfium.so is a shared object anyway.
#
# -rpath $ORIGIN so the binary finds libpdfium.so sitting next to it in
# /userdata/bd-play, with no LD_LIBRARY_PATH in the systemd unit.
zig cc -target aarch64-linux-gnu.2.28 \
  -O2 \
  -I "$SDK/include" \
  -o "$DIST/bdpdf-linux-arm64" \
  "$HERE/bdpdf.c" \
  -L "$SDK/lib" -lpdfium \
  -Wl,-rpath,'$ORIGIN' \
  -lm

cp "$SDK/lib/libpdfium.so" "$DIST/libpdfium.so"

# The shipped .so carries debug info; strip it. Halves what the browser has to
# serve and what /userdata has to hold.
if command -v llvm-strip >/dev/null; then
  llvm-strip --strip-debug "$DIST/libpdfium.so" 2>/dev/null || true
fi

echo "built:  $DIST/bdpdf-linux-arm64"
file "$DIST/bdpdf-linux-arm64"
echo "size:   $(du -h "$DIST/bdpdf-linux-arm64" | cut -f1)"
echo "pdfium: $(du -h "$DIST/libpdfium.so" | cut -f1)  ($(cat "$SDK/VERSION" | tr '\n' ' '))"
