# Notes

Working notes for this repo: status, decisions, and the traps that have actually bitten.
Migrated out of Claude Code's memory on 2026-08-24, so they are written in the first
person and dated by when each thing was learned — that date is usually the useful part.

Cross-cutting notes that are not specific to this repo live in
[fleet-notes](https://github.com/stoatworks-labs/fleet-notes).

*bd-play-usb-player (bdplay) - USB media player for the BirdDog PLAY; PUBLIC repo, shipped by the PLAY Patcher, working on hardware*

`~/Projects/bd-play-usb-player` — **PUBLIC** (github.com/stoatworks-labs/**bd-play-usb-player**,
MIT, branch `main`), USB media player for the BirdDog PLAY. **Shipped by
[birddog play patcher](https://github.com/stoatworks-labs/birddog-play-patcher/blob/main/docs/NOTES.md) (`birddog-play-patcher`)** as its "USB media player" checkbox — the
patcher does NOT vendor the source (unlike bdkvm's `agent/`, which must because
birddog-re is private); it links here and syncs only `player/dist/` binaries, so
there is no second copy to drift.
Built 2026-08-13, **working end to end on unit `.42`** (fw 1.0.30). Adds a
**USB** entry to birdUI's source dropdown and plays video/stills/PDF off a
stick, hardware-decoded straight to HDMI, in order or random, looping.

Go, `CGO_ENABLED=0` — **no zig needed**, unlike [birddog re](https://github.com/stoatworks-labs/birddog-re/blob/main/docs/NOTES.md) (`birddog-re`)'s bdkvm
and bdcam, because bdplay dlopens nothing; it drives `gst-launch-1.0` and
`mount` as subprocesses. Sibling repo, packaged by `fwbuild --with-play` /
`--with-play-pdf` from `../bd-play-usb-player/dist/bdplay-linux-arm64`. **Repo is
`bd-play-usb-player`; binary/service/command are all `bdplay`** — keep the
checkout dir named after the repo or sibling paths break.

**The device already has a complete media stack — this corrects a wrong belief
in [birddog re](https://github.com/stoatworks-labs/birddog-re/blob/main/docs/NOTES.md) (`birddog-re`).** That note says "no RGA, no GStreamer"; that is true
of what **`PPApp` links**, NOT of the rootfs. The stock rootfs has **GStreamer
1.14.4** with **`rockchipmpp`** (`mppvideodec`, `mpph264enc`, `mppjpegdec/enc`)
and **`kmssink`**. Measured: 1080p H.264 plays in real time at **~11% of one
core**, VOP confirmed scanning out NV12 on `win1-0` at 1920x1080p60. HDMI audio
is **card 1** (`hw:1,0`); card 0 is SoC i2s and goes nowhere.

**The integration hinges on one fact: `videoset.html` is a Go template parsed
FROM DISK ON EVERY REQUEST.** Verified by editing it and seeing the change
served with no restart, no recompile. So the USB option is just added to the
file.
> **DISPUTED 2026-08-13 — do not rely on this without re-testing.**
> [bdcam](https://github.com/stoatworks-labs/bdcam/blob/main/docs/NOTES.md) (`bdcam`)'s UVC tab landed the opposite hardware observation on a
> structurally identical template: the tab only appeared after restarting the
> right unit (`BirdDogWebUI`, not `birddog-web-ui`), which reads as parsed at
> STARTUP. Both are hardware observations. Until it is settled, assume a restart
> is needed and use a health check with rollback — correct under either answer.
> See [bdts](https://github.com/stoatworks-labs/bdts/blob/main/docs/NOTES.md) (`bdts`).
- **You CANNOT make the stock handler understand "USB".** `birddog-web-ui`
  validates `SourceSelection` against its own NDI/CloudConnect/SRT allowlist,
  and the Express API on :8080 validates every other `/decodesetup` field but
  has **no branch for `SourceSelection` at all**. So the patch also injects a
  script that intercepts the select's change event and drives bdplay instead.
  BirdDog's stored `SourceSelection` is never set to USB — the device stays
  configured for NDI underneath, which is what makes going back instant.
- Go's `html/template` passes the injected `<script>` through **verbatim** — no
  `ZgotmplZ` escaping damage. Verified by injecting it into `dashboard.html`
  (which serves unauthenticated) and diffing the served output.
- Patch is idempotent, keeps `videoset.html.bdplay-stock`, writes atomically,
  and `bdplay -unpatch-ui` restores it **byte-identical to
  `/userdata/videoset.html.orig`** (BirdDog's own factory backup).

**Display handover — the brick risk.** PPApp holds DRM master; there is one and
no sharing. USB mode = `systemctl stop BirdDogRunner`. **`BirdDogRunner` is
`Restart=always`, so killing PPApp directly just gets it restarted a second
later** — always go through systemd. Every exit path restores it, including
SIGTERM: verified `systemctl restart bd-play` mid-playback recovers the display.
A unit with PPApp dead has dark HDMI + working web UI and reads as bricked.

**CYCLING BirdDogRunner DESTABILISES IT — measured, and it is our doing.**
`birddog-runner` aborts in its own C++ during NDI receiver teardown
(`terminate called without an active exception`, `status=6/ABRT`). On unit `.42`:
**0 aborts in the journal before bdplay was installed at 14:35, 30 after.**
- **Per-handover verification kept passing and still missed it** — PPApp always
  came back immediately; the abort happens minutes later. The damage is
  cumulative, so check `journalctl -u BirdDogRunner | grep -c "Failed with
  result"` across a session, not just PPApp right after a switch.
- **Once systemd rate-limits the failing unit, plain `systemctl start` is
  REFUSED and the unit stays dark** — the exact brick-look this daemon exists to
  prevent, and it was reachable. `Release()` now runs **`reset-failed` first**
  and then **polls for PPApp**, because `systemctl start` exiting 0 only means
  the job was accepted.
- **Do NOT induce runner crashes to test the recovery path** — that is the same
  harm the rule prevents. `systemctl kill -s SIGABRT` in a loop does not even
  reach the rate-limited state (the unit's `ExecStartPre` sleep keeps it
  `activating`).

**Storage:** usb-storage is built into the kernel; **vfat and ntfs are built in,
exFAT is NOT**, and there are no loadable modules. **Nothing automounts**:
`/media/usb0..7` exist but no udev rule or daemon populates them, so bdplay
mounts them itself, always **read-only** (people unplug these without ejecting).
Volumes are filtered to real USB devices via `/sys/class/block/<n>` containing
`/usb`.

**exFAT is SOLVED and needs NO kernel rebuild and NO touching the OS image** —
the kernel already has **FUSE**: `/dev/fuse` exists (10,229) and opens, and both
`fuse` and `fuseblk` are registered. So exFAT is purely userspace. Verified on
hardware 2026-08-13: a real exFAT volume mounts ro via a shipped static
`mount.exfat-fuse` and reads back **byte-identical md5**; `umount` works on the
fuseblk mount. `blkid` (util-linux 2.33.1) already knows `exfat`, so type
detection needed nothing. **Loop devices work** (`/dev/loop0` + `losetup`) —
that is how it was tested with no physical stick, using an image made on macOS
with `hdiutil create -fs exFAT -layout NONE` (which is already a raw fs image).
- Build traps in `tools/build-exfat.sh`: the device has **neither libfuse nor
  fusermount**, so link fully static against a cross-built libfuse 2.9.9;
  **pin fuse-exfat 1.3.0, NOT 1.4.0** (1.4.0 migrated to FUSE 3 which prefers a
  `fusermount3` helper — libfuse 2.x's `fuse_kern_mount()` tries direct
  `mount(2)` first and we run as root, so no helper ships); and set
  **`PKG_CONFIG_LIBDIR`** to the cross prefix or configure links the target
  against the Mac's own `/usr/local` fuse3. Strip via `LDFLAGS="-static -s"` —
  **`zig objcopy --strip-all` is "unimplemented"**. 312 KB stripped.
- Licence: fuse-exfat **GPL v2**, libfuse **LGPL v2.1** — copyleft, source
  offer, but **no network clause**, so materially lighter than mutool's AGPL.

**PDF: PDFium is the default, MuPDF a fallback.** `pdf.go` picks `bdpdf` first
and only falls back to `mutool` if absent — **do not invert that**, it would put
an AGPL binary back on the default path. `/api/status` reports `pdf_backend`.

**`bdpdf` (`bdpdf/`) is a ~150-line C wrapper around PDFium** — the answer to
the whole licence problem. **PDFium is BSD-3**, and
`bblanchon/pdfium-binaries`' prebuilt **`pdfium-linux-arm64`** needs only
**GLIBC_2.17** (device has 2.28) with NEEDED = libpthread/libm/libgcc_s/libc/
loader and **no libstdc++** (it static-links its own C++ runtime). 7.5 MB .so +
132 KB wrapper vs mutool's 37 MB. Verified on hardware 2026-08-13: 3 pages at
1920x1080 in **1.3 s**, pixel-correct.
- **Take the GNU build, NOT the musl one** — `pdfium-linux-musl-arm64`'s .so has
  `NEEDED: libc.so` and wants musl's loader, which Debian 10 does not have.
- Link with `-Wl,-rpath,'$ORIGIN'` so `bdpdf` finds `libpdfium.so` beside it
  with no `LD_LIBRARY_PATH` in the unit.
- PNG out via **stb_image_write** (public domain, single header) — no libpng on
  the device. **Trap: do NOT `#define STBI_WRITE_NO_STDIO 0`** — stb tests it
  with `#ifndef`, so defining it *at all* removes `stbi_write_png` and the build
  fails on an implicit declaration.
- PDFium hands back **BGRA**; repack to RGB (verified by colour, not just by it
  rendering — a wrong swap still produces a plausible image).

**mutool is AGPL v3 and must NEVER ship from the public web patcher** — §13
reaches network users, which a web-driven player plainly has, and at 37 MB it
exceeds **Cloudflare Workers static assets' 25 MiB per-file limit** anyway. Its
cross-build (`tools/build-mutool.sh`) has three traps worth keeping: **glibc
cannot be statically linked** (zig refuses — use musl); **`HAVE_LIBCRYPTO=no`**
or the link fails on undefined `X509_*` from PKCS7; and **`CXX` must be set as
well as `CC`** — harfbuzz is C++, and the host compiler silently emits Mach-O
objects lld skips, surfacing much later as undefined `fzhb_*`.

**PowerPoint is deliberately out of scope** — LibreOffice will not fit or
perform on 2 GB + 4x A53, and a partial OOXML renderer misdraws SmartArt,
charts and fonts. Office files are listed with "export to PDF" rather than
silently ignored.

Design notes worth not re-deriving: **one gst process per item** (the 2020
`rockchipmpp` plugin does not renegotiate caps gracefully mid-pipeline; ~300 ms
of black is the price); **SIGINT before SIGKILL** or the DRM plane stays
configured and the next `kmssink` fails to set a mode; **random reshuffles a
permutation per cycle** rather than picking independently, so nothing starves,
and the reshuffle never puts the just-played item first.

**TV-remote mode (2026-09-15, built from the firmware, NOT yet run on a PLAY —
`birddog-play-42` had been offline a month).** The question was whether the
features we patch in can be reached from the PLAY's own HDMI OSD via CEC. What
the 1.0.34 `PPApp` disassembly settled:
- **Stock CEC is PPApp's and only PPApp's.** With no arguments it opens
  `/dev/cec0` as `CEC_S_MODE = 0x31` (INITIATOR | EXCL_FOLLOWER_PASSTHRU),
  claims a Playback logical address (empty OSD name) and **never calls
  CEC_TRANSMIT** — so it answers no <Give OSD Name>/<Give Physical Addr> and
  sends no <Active Source>; only the kernel's <Report Physical Address> at
  claim time tells the TV where it is. That is why BirdDog's own manual says
  "try all CEC modes". Its OSD driver maps **exactly** UI codes 0x00 Select,
  0x01 Up, 0x02 Down, 0x03 Left, 0x04 Right (Left/Right = page between the
  NDI / SRT / CloudConnect lists); everything else is dropped. Any key shows
  the OSD, ~5 s idle hides it. `--enable-ir` and `--osd-kb` drivers exist but
  are off. ioctl sizes seen: caps 0x4c, msg 0x38, log_addrs 0x5c, event 0x50 —
  mainline UAPI, pinned in `cec_test.go`.
- **The OSD is a source list drawn from files**, so the one text we can put on
  it is a source *name*: an entry in `/etc/birddog_srt_src.json` shows on the
  SRT page. Picking it makes PPApp dial the URI as an SRT **caller** — the URI
  must be exactly `host:port?mode=caller&latency=N` (the stock page parses it
  by `split("&")[1]`), and the name must have **no spaces or punctuation**
  (`$('#'+name)` in the stock page; bd-play-stream-gateway hit the colon case).
  Hence `USB-Player` → `127.0.0.1:9107`, and the "listener" is a bare UDP
  socket: the first handshake datagram is the trigger. No SRT is spoken.
- **Exclusive follower means nobody else can follow while PPApp runs**, but
  monitors (root) still can, and once PPApp is stopped for the display the
  adapter is free — which is why the browser lives in bdplay: it already owns
  the moments when PPApp is gone. `Display` grew `Hold`/`Unhold` (a pin that
  makes `Release` a no-op) so a playlist ending inside the session returns to
  the browser instead of bouncing the display through PPApp.
- **The decoder persists its menu choice.** Assumed, not proven — but the safe
  assumption: if PPApp restarted with our entry selected it would dial the
  trigger again at once. `sourceGuard` polls `/etc/birddog-source1-name` and
  `SourceSelection`, remembers the last non-USB pair, and writes it back
  before `BirdDogRunner` starts; with no history it sets NDI. The trigger also
  ignores datagrams for 15 s after a session (PPApp retries its connect).
- **Drawing the browser**: PPApp is DRM master and 4.4 has no DRM leases, so
  frames go through one long-lived `gst-launch fdsrc ! rawvideoparse !
  videoconvert ! kmssink sync=false` fed RGBA on stdin (`rawvideoparse`
  because a pipe delivers short reads and fdsrc alone would hand kmssink
  fragments). It is probed with `gst-inspect-1.0 --exists`; the fallback is
  `multifilesrc loop=true` re-reading a PNG we replace atomically. Only one
  kmssink can own the plane, so the browser pipeline is stopped before every
  playback and restarted after. Text is `golang.org/x/image` + the Go fonts
  (BSD) — no arrows or media glyphs in those faces, so the legend draws dots
  and the rows draw their own icons.

**Hardware checklist for the first run** (in order; each one can fail alone):
1. `bdplay` start log: `added "USB-Player" to the decoder's on-screen source
   list` and nothing about the trigger port. `cat /etc/birddog_srt_src.json`.
2. On the TV: Left/Right to the SRT page, `USB-Player` listed, OK on it. Log:
   `trigger: 64-byte datagram from 127.0.0.1:…`, then `stopping
   BirdDogRunner`. If PPApp is *not* a caller for that URI shape, nothing
   arrives — check with `tcpdump -i lo udp port 9107`.
3. `cec: claimed logical address N as "USB Player"` — if `CEC_S_MODE` fails
   with EBUSY, PPApp was still alive (Hold's 10 s wait was not enough); if
   "no logical address after 4s", the adapter has no physical address (EDID).
4. The browser on HDMI via the raw pipeline (`remote: browser on screen at
   WxH via the raw pipeline`). If black: `gst-launch-1.0 fdsrc fd=0 !
   rawvideoparse … ! kmssink` by hand with `/dev/zero`; try `-osd-pipeline=png`.
5. Which keys the TV forwards: `journalctl -u bd-play | grep 'cec:'` shows
   `ignoring user control 0x..` for anything unmapped. Arrows/OK are the
   floor; check Exit, Play/Stop, skip, colour keys per TV.
6. Leaving (Red): `decoder source restored to …`, PPApp back, and — the loop
   test — it must **not** re-enter by itself within 30 s. If it does, the
   guard's restore did not take: compare `/etc/birddog-source1-name` before
   and after.
7. Idle 4K panels: the frame is rendered at the detected mode (33 MB at 4K);
   if `Show` is slow, render at 1080p and rely on kmssink `can-scale`.

Related: [birddog re](https://github.com/stoatworks-labs/birddog-re/blob/main/docs/NOTES.md) (`birddog-re`), [birddog play patcher](https://github.com/stoatworks-labs/birddog-play-patcher/blob/main/docs/NOTES.md) (`birddog-play-patcher`),
**cosession shared checkout** (working-practice note, kept in Claude memory).
