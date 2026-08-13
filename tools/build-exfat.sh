#!/usr/bin/env bash
# Cross-compile a static `mount.exfat-fuse` for the BirdDog PLAY.
#
# WHY THIS IS A USERSPACE PROBLEM, NOT AN IMAGE ONE
# -------------------------------------------------
# The PLAY's kernel has no exfat driver and ships **no loadable modules at all**,
# so there is no module to insmod. That would normally mean rebuilding the
# kernel and reflashing the boot partition. It does not here, because the
# kernel already has **FUSE**: /dev/fuse exists (10,229), `fuse` and `fuseblk`
# are both registered in /proc/filesystems, and /dev/fuse opens fine. So exFAT
# can be driven entirely from userspace and the OS image is never touched.
#
# The device has neither libfuse nor fusermount, so both have to be supplied.
# Everything is linked statically into the one binary:
#   * no libfuse.so to install alongside it,
#   * no fusermount helper — libfuse 2.x's fuse_kern_mount() tries the direct
#     mount(2) syscall first and only falls back to fusermount if that fails,
#     and bdplay runs as root, so the direct path always wins.
#
# LICENCE: fuse-exfat is **GPL v2** and libfuse 2.x is **LGPL v2.1**. bdplay
# exec()s the mount helper as a separate process, so bdplay is not a derived
# work — but distributing this binary carries a source-offer obligation. That is
# a much lighter obligation than mutool's AGPL (no network clause), but it is
# still copyleft. Like mutool, this is NOT built by build.sh and NOT vendored.
set -euo pipefail

FUSE_VERSION="${FUSE_VERSION:-2.9.9}"
# 1.3.0, NOT 1.4.0: relan/exfat 1.4.0 migrated to FUSE 3, and fuse3's mount
# path prefers the fusermount3 helper, which we would then also have to ship.
# 1.3.0 links libfuse 2.x, whose fuse_kern_mount() tries the direct mount(2)
# syscall first — and we always run as root, so that path always succeeds and
# no helper binary is needed on the device.
EXFAT_VERSION="${EXFAT_VERSION:-1.3.0}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="${WORK:-$HERE/../build/exfat}"
OUT="$HERE/../dist/mount.exfat-fuse-linux-arm64"

command -v zig >/dev/null || { echo "error: zig not found (brew install zig)" >&2; exit 1; }

TARGET="aarch64-linux-musl"
JOBS="$(sysctl -n hw.ncpu 2>/dev/null || nproc)"
PREFIX="$WORK/prefix"

mkdir -p "$WORK" "$PREFIX"
cd "$WORK"

export CC="zig cc -target $TARGET"
export AR="zig ar"
export RANLIB="zig ranlib"
# Isolate pkg-config from the build host. Without PKG_CONFIG_LIBDIR pointing
# only at our prefix, exfat's configure happily finds the Mac's own
# /usr/local/lib/pkgconfig and links the target binary against host libraries.
export PKG_CONFIG_LIBDIR="$PREFIX/lib/pkgconfig"
export PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig"
# musl has no __off64_t etc; libfuse wants large-file support declared.
export CFLAGS="-O2 -D_FILE_OFFSET_BITS=64 -D_GNU_SOURCE"

# ---------------------------------------------------------------- libfuse 2.x
if [ ! -f "$PREFIX/lib/libfuse.a" ]; then
  FUSE_TAR="fuse-${FUSE_VERSION}.tar.gz"
  [ -f "$FUSE_TAR" ] || curl -fSL -o "$FUSE_TAR" \
    "https://github.com/libfuse/libfuse/releases/download/fuse-${FUSE_VERSION}/${FUSE_TAR}"
  rm -rf "fuse-${FUSE_VERSION}"
  tar xf "$FUSE_TAR"
  cd "fuse-${FUSE_VERSION}"

  # --disable-util drops fusermount and mount.fuse, which we neither need (root
  # mounts directly) nor can install on the device anyway.
  ./configure \
    --host=aarch64-linux-musl \
    --prefix="$PREFIX" \
    --enable-static --disable-shared \
    --disable-util --disable-example \
    UDEV_RULES_PATH="$PREFIX/udev" INIT_D_PATH="$PREFIX/init.d"

  make -j"$JOBS"
  make install
  cd "$WORK"
fi

# ---------------------------------------------------------------- fuse-exfat
EXFAT_TAR="fuse-exfat-${EXFAT_VERSION}.tar.gz"
[ -f "$EXFAT_TAR" ] || curl -fSL -o "$EXFAT_TAR" \
  "https://github.com/relan/exfat/releases/download/v${EXFAT_VERSION}/${EXFAT_TAR}"
rm -rf "fuse-exfat-${EXFAT_VERSION}"
tar xf "$EXFAT_TAR"
cd "fuse-exfat-${EXFAT_VERSION}"

# Static all the way through: the point is a single file that needs nothing on
# the device.
./configure \
  --host=aarch64-linux-musl \
  --prefix="$PREFIX" \
  --enable-static --disable-shared \
  FUSE_CFLAGS="-I$PREFIX/include/fuse -D_FILE_OFFSET_BITS=64" \
  FUSE_LIBS="$PREFIX/lib/libfuse.a -lpthread" \
  LDFLAGS="-static -s"

make -j"$JOBS"

mkdir -p "$(dirname "$OUT")"
cp fuse/mount.exfat-fuse "$OUT"
echo "built: $OUT"
file "$OUT"
echo "size:  $(du -h "$OUT" | cut -f1)"
