#!/usr/bin/env bash
# Cross-compile a static `mutool` for the BirdDog PLAY (aarch64, Debian 10).
#
# The PLAY's rootfs has no PDF renderer at all, so PDF playback needs one
# shipped. MuPDF is the pick because `mutool draw` is a single self-contained
# binary with no runtime dependencies once statically linked — no poppler/glib
# stack to drag onto a 3.5 GB rootfs.
#
# LICENCE, READ THIS BEFORE SHIPPING: MuPDF is **AGPL v3** (or a paid Artifex
# commercial licence). bdplay only ever exec()s it as a separate process, so
# bdplay itself is not a derived work — but *distributing* the binary carries
# the AGPL's source-offer obligation, and AGPL section 13 covers users
# interacting with it over a network. That is why mutool is NOT vendored into
# this repo and NOT built by build.sh: it is an opt-in extra you fetch and
# build deliberately. If bdplay is ever published, resolve this first.
#
# There is no container runtime on the build host, so this cross-compiles
# directly with zig as the C compiler, the same approach bdkvm and bdcam use.
set -euo pipefail

MUPDF_VERSION="${MUPDF_VERSION:-1.24.9}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="${WORK:-$HERE/../build/mupdf}"
OUT="$HERE/../dist/mutool-linux-arm64"

command -v zig >/dev/null || { echo "error: zig not found (brew install zig)" >&2; exit 1; }

mkdir -p "$WORK"
cd "$WORK"

TARBALL="mupdf-${MUPDF_VERSION}-source.tar.gz"
SRC="mupdf-${MUPDF_VERSION}-source"

if [ ! -d "$SRC" ]; then
  [ -f "$TARBALL" ] || curl -fSL -o "$TARBALL" "https://mupdf.com/downloads/archive/${TARBALL}"
  tar xf "$TARBALL"
fi

cd "$SRC"

# Stage 1: MuPDF generates C source (font dumps, CMap tables) using tools it
# compiles for the BUILD host. Those must be native, so run this step with the
# system compiler before switching to the cross compiler.
#
# `generated/` is host-independent and slow to rebuild, so it is kept; only the
# target output is cleaned.
make -j"$(sysctl -n hw.ncpu 2>/dev/null || nproc)" generate

# Drop any previous target build. Without this a rebuild silently reuses objects
# compiled under different flags — an archive still holding pkcs7-openssl.o from
# an earlier run fails the link on undefined X509_* symbols even after OpenSSL
# has been switched off.
rm -rf build/release

# Stage 2: the target build, against **musl** rather than glibc.
#
# glibc cannot be fully statically linked — zig refuses outright with "libc of
# the specified target requires dynamic linking", which is what a first attempt
# at aarch64-linux-gnu.2.28 hits at the final link step. musl static links
# cleanly and, because the result depends on no shared library at all, it drops
# the whole question of matching the device's 2019-vintage glibc. mutool is a
# standalone CLI we only ever exec(), so there is nothing to gain from glibc.
# CXX matters as much as CC: harfbuzz is C++, and leaving CXX at the host
# default compiles those sources to Mach-O objects that lld then silently
# skips — which surfaces much later as undefined fzhb_* symbols at the final
# link, looking like a missing library rather than a wrong compiler.
TARGET="aarch64-linux-musl"
export CC="zig cc -target $TARGET"
export CXX="zig c++ -target $TARGET"
export LD="zig c++ -target $TARGET"
export AR="zig ar"
export RANLIB="zig ranlib"

# muraster is built by the `tools` target and is not wanted; build mutool
# explicitly so a failure elsewhere in the tools set does not block us.
#
# HAVE_LIBCRYPTO=no drops MuPDF's PKCS7 support, which is digital-signature
# verification for signed PDFs and needs OpenSSL. We only rasterise pages, and
# there is no OpenSSL for the target, so leaving it on fails the final link on
# undefined X509_* symbols. USE_SYSTEM_LIBS=no forces the bundled freetype,
# jbig2dec, openjpeg, harfbuzz and zlib rather than the build host's macOS ones.
make -j"$(sysctl -n hw.ncpu 2>/dev/null || nproc)" \
  build=release \
  OS=Linux \
  HAVE_X11=no HAVE_GLUT=no HAVE_CURL=no HAVE_OBJCOPY=no \
  HAVE_LIBCRYPTO=no \
  USE_SYSTEM_LIBS=no \
  XCFLAGS="-static -Os" \
  XLIBS="-static" \
  build/release/mutool

mkdir -p "$(dirname "$OUT")"
cp build/release/mutool "$OUT"
echo "built: $OUT"
file "$OUT"
echo "size:  $(du -h "$OUT" | cut -f1)"
