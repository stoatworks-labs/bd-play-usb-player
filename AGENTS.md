# AGENTS.md — bdplay

USB media player for the **BirdDog PLAY** (Rockchip RK3328, quad A53, Debian 10
aarch64). Adds a **USB** entry to birdUI's source dropdown and plays video,
stills and PDFs off a USB stick, straight to HDMI. Go, cross-compiled for the
device. **Private repo** — see the licence note below before considering
otherwise.

Start with [`README.md`](README.md) for the design and the hardware
measurements behind it. This file is the operating rules.

## Where this sits

`bdplay` is the canonical, standalone home of this program. The reverse
engineering that justifies every claim in the README lives in
`~/Projects/birddog-re` — **local-only, no remote, and it must stay that way**:
its history contains a recovered vendor AES key and a decrypted firmware blob.
Do not push `birddog-re` anywhere, and do not copy key material or vendor
firmware into this repo.

Sibling checkouts under `~/Projects` are assumed: `birddog-re` (research),
`bdkvm` (NDI KVM), `bdcam` (UVC → NDI). `birddog-re`'s `tools/fwbuild` builds
the installable `.fw` and finds this repo's binary at
`../bdplay/dist/bdplay-linux-arm64` (override with `BDPLAY=`).

## Hard rules

1. **Never leave the unit without a display owner.** bdplay stops
   `BirdDogRunner.service` to take DRM master and must restart it on every exit
   path — normal stop, SIGTERM, panic, failed acquire. A unit whose PPApp is
   dead has a dark HDMI output and a working web UI, which reads to a user as
   bricked. `display.go` owns this; if you add an early return to the playback
   loop, check the `defer p.display.Release()` still covers it.
2. **Never modify a firmware file without a backup and an atomic write.**
   `videoset.html` is 370 KB of Go template that the whole web UI depends on;
   a truncated write takes birdUI down entirely. `patch.go` writes via temp
   file + rename and keeps `videoset.html.bdplay-stock`.
3. **Do not vendor mutool or mount.exfat-fuse, and do not add either to
   `build.sh`.** Both are copyleft (see below). `tools/build-mutool.sh` and
   `tools/build-exfat.sh` are deliberately separate, opt-in steps.
4. **Build only through `./build.sh`**, or with `GOOS=linux GOARCH=arm64
   CGO_ENABLED=0` set. A host build silently produces a macOS binary that will
   not run on the device.
5. **Never write to the USB stick.** Volumes are mounted read-only on purpose —
   people unplug these without ejecting, and a read-only mount cannot be
   corrupted by that.

## Licence: three helpers, none vendored

bdplay `exec()`s all of these as separate processes and shares no address
space, so bdplay itself is not a derived work of any of them. **Distributing
the binaries is what carries the obligation.** None is in this repo or produced
by `build.sh`.

- **`bdpdf` + `libpdfium.so` — ours MIT, PDFium BSD-3-Clause.** The default PDF
  path, and the only one that can ship from the public web patcher. Permissive,
  ~7.6 MB, prebuilt linux-arm64 needing only GLIBC_2.17.
- **`mount.exfat-fuse` — fuse-exfat GPL v2, libfuse LGPL v2.1.** Source offer,
  but **no network clause**. Shipped by `--with-play` whenever built, because
  without it most sticks over 32 GB do not mount at all.
- **`mutool` — MuPDF, AGPL v3.** Kept as a fallback for units that already have
  it. **Never ship this from the web patcher**: §13 reaches users interacting
  over a network, which a web-driven signage player plainly does, and at 37 MB
  it exceeds Cloudflare's 25 MiB per-file asset limit regardless. Local
  `fwbuild --with-play-pdf` only.

**Preference order is PDFium first, mutool only if `bdpdf` is absent.** Do not
invert it — that would quietly put an AGPL binary back on the default path.

Everything else in the runtime is already on the device (GStreamer, the
Rockchip MPP plugins) and is not redistributed by us.

## Hardware facts this depends on

All measured on unit `.42`, firmware 1.0.30, 2026-08-13. Re-verify before
assuming they hold on another firmware.

- GStreamer **1.14.4** is on the stock rootfs with the **`rockchipmpp`** plugin
  (`mppvideodec`, `mpph264enc`, `mppjpegdec`, `mppjpegenc`) and **`kmssink`**.
  `birddog-re`'s note that there is "no GStreamer" refers to what `PPApp`
  links, not what the rootfs carries — do not repeat that mistake.
- Hardware decode measured: a 1080p H.264 clip plays in real time at **~11% of
  one core**, VOP confirmed scanning out NV12 on `win1-0` at 1920x1080p60.
- HDMI audio is **card 1** (`rockchiphdmi`, `hw:1,0`). Card 0 is the SoC i2s and
  goes nowhere on this board.
- **usb-storage is built into the kernel** (there are no loadable modules at
  all). **vfat and ntfs are built in; exfat is NOT.** But **FUSE is fully
  present** — `/dev/fuse` exists (10,229) and opens, and both `fuse` and
  `fuseblk` are registered — so exFAT is a **userspace** problem and needs no
  kernel rebuild and no touching the OS image. Verified on hardware: a real
  exFAT volume mounts read-only via the shipped helper and reads back
  byte-identical. `blkid` (util-linux 2.33.1) already knows `exfat`, so type
  detection needed nothing.
- The device has **neither libfuse nor fusermount**, so the helper is linked
  fully static (musl). libfuse 2.x's `fuse_kern_mount()` tries the direct
  `mount(2)` syscall before falling back to fusermount, and we run as root, so
  no helper binary is needed. **This is why the build pins fuse-exfat 1.3.0 and
  not 1.4.0** — 1.4.0 migrated to FUSE 3, which prefers `fusermount3`.
- **Loop devices work** (`/dev/loop0`, `losetup` present), which is how exFAT
  was tested without a physical stick.
- Nothing automounts. `/media/usb0..usb7` exist but no udev rule or daemon
  populates them, so bdplay mounts them itself.
- **`videoset.html` is parsed from disk on every request** — verified by
  editing it and seeing the change served with no restart. This is what makes
  the dropdown integration possible at all.
- The compiled Go binary validates `SourceSelection` against its own
  NDI/CloudConnect/SRT allowlist, and the Express API on :8080 has no branch
  for the field at all. **You cannot make the stock handler understand "USB"** —
  the injected script intercepts the change event instead.

## Layout

```
main.go        flags, wiring, signal handling
storage.go     USB discovery, mounting, eject
library.go     scanning a volume into playable items
playlist.go    ordering: sequential / random, looping
player.go      gst-launch supervision, one process per item
display.go     DRM handover with PPApp
pdf.go         page rendering + cache; PDFium backend, mutool fallback
bdpdf/         the PDFium CLI wrapper (C) + its cross-build
api.go         REST API
ui.go          injected birdUI script + standalone control page
patch.go       videoset.html patch / unpatch
packaging/     systemd unit + installer
tools/         build-mutool.sh (opt-in, AGPL)
```

## Testing

`go test ./...` covers the parts that are pure logic and easy to get subtly
wrong: natural sort, library scanning and selection (including traversal
refusal), playlist permutation properties, URI escaping, and the birdUI
patch/unpatch round trip. Everything else needs the device.

There is no emulation path — RK3328, MPP and KMS cannot be faked. Use
`-media-dir` to exercise the player from internal storage without a stick.
