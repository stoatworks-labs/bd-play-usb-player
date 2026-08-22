# bdplay user guide

bdplay adds a **USB** entry to the BirdDog PLAY's source dropdown and **plays video, stills and
PDFs from a USB stick straight out of HDMI** — hardware decoded, in order or shuffled, looping
until you stop it.

The PLAY is an NDI/SRT *decoder*: it has no notion of local media. This turns one into a signage
player **without giving up anything** — select NDI again and the stock decode path comes straight
back.

![bdplay's control page, showing a five-item library, transport controls, live status and log](control-ui.png)

> **Before you rely on this:** every claim here was **measured on a real BirdDog PLAY** — hardware
> video decode, HDMI audio, stills, PDF rendering, the exFAT mount and the display handover with
> BirdDog's own decoder are all confirmed working on hardware.
>
> That is **one unit on one firmware version.** It has not been tested across the range of PLAY and
> Pod firmwares in the field, and **USB hotplug with a physical stick is still unverified** —
> testing used a loop device and internal storage.
>
> This codebase was created with AI assistance, directed and reviewed by a human author.

---

## Installing

**The easy way is a checkbox in the [PLAY Patcher](https://birddog-play-patcher.stoatworks-labs.com)**,
which assembles the firmware package in your browser — no toolchain, nothing to build.

![The USB media player option in the PLAY Patcher](patcher-option.png)

To install by hand, copy the binary and its service unit onto the unit over SSH (**port 9031** on
stock BirdDog firmware) and enable the service.

On start it patches the video settings page to add the USB option. `bdplay -unpatch-ui` removes it
again — the stock page is untouched apart from one added option.

---

## Using it

Open the PLAY's own web UI, go to the video settings page, and pick **USB** from Source Selection.
A panel appears with the stick's contents.

![The USB media player panel inside the stock AV Setup page](birdui-panel.png)

**There is also a standalone control page on port 8091**, which additionally shows the log — **use
that if the injected panel does not appear**, and use it first when something will not play,
because the log answers most of those questions.

Everything is drivable from the API too, so a Companion button or a cron job works.

---

## What it plays

| Type | Formats | How |
|---|---|---|
| **Video** | mp4, mov, mkv, avi, ts, m2ts, mpg, webm, wmv | Hardware H.264/HEVC/VP8/VP9 — **~11% of one core at 1080p** |
| **Stills** | jpg, png, bmp, gif, webp, tif | Scaled to the panel with aspect preserved, held for a configurable dwell |
| **PDF** | pdf | Pages pre-rendered to images, then played as stills — 3 pages at 1080p in ~1.3 s |
| **Office** | pptx, docx, … | **Not rendered.** Listed with "export to PDF" rather than silently ignored |

**PowerPoint is deliberately out of scope.** Rendering it on-device would mean either a full office
suite — which will not fit or perform on 2 GB and four small cores — or a partial renderer that
misdraws SmartArt, charts and embedded fonts, **looking broken in exactly the situations where a
deck matters.** Exporting to PDF is one step and renders exactly as authored.

---

## Playback options

- **Selection** — one file, one folder (optionally including subfolders), or the whole stick.
- **Order** — as listed (**natural sort, so `slide2` precedes `slide10`**) or random.
- **Loop** — on by default.
- **Dwell** — how long each still or PDF page holds the screen.

**Random reshuffles the whole set each cycle** rather than picking independently, so every file
plays once per cycle instead of some repeating while others starve — and **the reshuffle never puts
the item that just played at the front of the next cycle.**

---

## If something will not play

| Symptom | Cause |
| --- | --- |
| **The USB option does not appear** | The UI patch did not land. Use the standalone control page on port 8091. |
| **A file is listed but will not play** | Read the log on that page — it answers most of these. |
| **A PowerPoint file is listed and does nothing** | Deliberate. Export it to PDF. |
| **The stick was not picked up** | Hotplug with a physical stick is the one path still unverified. Re-seat it, or reboot with it in. |
| **NDI stopped when I selected USB** | Correct — the stock decoder hands over the display. Select NDI again and it comes back. |
