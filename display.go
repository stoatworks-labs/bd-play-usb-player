package main

// Display arbitration.
//
// PPApp holds DRM master on /dev/dri/card0 for as long as it runs, and the
// kernel allows exactly one master. So kmssink cannot open the display while
// PPApp is alive — there is no sharing arrangement to negotiate, and no way to
// composite our output over its. Entering USB mode therefore means stopping
// BirdDog's decode stack, and leaving it means starting it again.
//
// Two traps, both learned the hard way on hardware:
//
//   - BirdDogRunner.service is Restart=always. Killing PPApp directly gets it
//     restarted underneath us a second later, which steals DRM back mid-clip.
//     Always go through systemctl stop, which tells systemd we meant it.
//
//   - birddog-update-wrapper stops BirdDogRunner and never restarts it. We are
//     not that; anything we stop, we restore, including on panic and on SIGTERM.

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const birdDogRunner = "BirdDogRunner.service"

// Display owns the handover of DRM master between PPApp and us.
type Display struct {
	mu sync.Mutex
	// held is true while we have stopped BirdDog's stack for our own use.
	held bool
	// wasActive records whether BirdDogRunner was running before we took over,
	// so that a device where the user had already stopped it stays stopped.
	wasActive bool
	log       *Logger
}

func NewDisplay(log *Logger) *Display { return &Display{log: log} }

// Acquire stops BirdDog's decode stack and waits for PPApp to release DRM.
func (d *Display) Acquire() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.held {
		return nil
	}

	d.wasActive = serviceActive(birdDogRunner)
	if d.wasActive {
		d.log.Info("stopping %s to take the display", birdDogRunner)
		if out, err := exec.Command("systemctl", "stop", birdDogRunner).CombinedOutput(); err != nil {
			return fmt.Errorf("stop %s: %v: %s", birdDogRunner, err, strings.TrimSpace(string(out)))
		}
	}

	// Wait for PPApp to actually exit. systemctl stop returns once systemd has
	// reaped the unit, but be defensive: a straggler still holding the DRM fd
	// makes kmssink fail with a bare "Could not open DRM module", which is an
	// unhelpfully generic error to debug from a log a week later.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processRunning("PPApp") {
			d.held = true
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Restore rather than leaving the box in a half-stopped state.
	d.restoreLocked()
	return fmt.Errorf("PPApp still running 10s after stopping %s", birdDogRunner)
}

// Release restarts BirdDog's decode stack if we stopped it.
func (d *Display) Release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restoreLocked()
}

func (d *Display) restoreLocked() {
	if !d.held {
		return
	}
	d.held = false
	if !d.wasActive {
		d.log.Info("%s was not running before we started; leaving it stopped", birdDogRunner)
		return
	}
	d.log.Info("restarting %s", birdDogRunner)

	// Clear a failed state first. BirdDog's runner aborts in its own C++ during
	// NDI receiver teardown (`terminate called without an active exception`,
	// SIGABRT), and cycling it — which is exactly what this player does — makes
	// that more likely. Once it has failed enough times systemd rate-limits it
	// and plain `start` is refused, so without reset-failed a unit can be left
	// permanently dark: HDMI black, web UI up, no decode. Harmless when the unit
	// is healthy.
	if out, err := exec.Command("systemctl", "reset-failed", birdDogRunner).CombinedOutput(); err != nil {
		d.log.Warn("reset-failed %s: %v: %s", birdDogRunner, err, strings.TrimSpace(string(out)))
	}

	if out, err := exec.Command("systemctl", "start", birdDogRunner).CombinedOutput(); err != nil {
		// Worth shouting about: this is the state that looks like a bricked
		// unit (dark HDMI, web UI up, no decode).
		d.log.Error("FAILED to restart %s: %v: %s — the unit will not decode NDI until this is fixed (systemctl reset-failed %s && systemctl start %s)",
			birdDogRunner, err, strings.TrimSpace(string(out)), birdDogRunner, birdDogRunner)
		return
	}

	// Confirm the decoder actually came back. `systemctl start` returning 0
	// only means systemd accepted the job; PPApp can still abort seconds later,
	// and a silent failure here is the one outcome users read as a brick.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if processRunning("PPApp") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	d.log.Error("%s was started but PPApp is not running after 15s — HDMI output will be dark. Check 'journalctl -u %s'",
		birdDogRunner, birdDogRunner)
}

// Held reports whether we currently own the display.
func (d *Display) Held() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.held
}

func serviceActive(unit string) bool {
	out, _ := exec.Command("systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out)) == "active"
}

func processRunning(name string) bool {
	// pgrep -x matches the exact process name, so "PPApp" does not also match
	// a shell whose command line mentions it.
	err := exec.Command("pgrep", "-x", name).Run()
	return err == nil
}

// Mode describes the connected display, used to render stills and PDF pages at
// the panel's native resolution rather than guessing 1080p.
type Mode struct {
	Width  int
	Height int
}

// DetectMode reads the current mode from the Rockchip VOP debug summary, which
// reports the real negotiated mode rather than the EDID's preferred one.
// Falls back to 1080p, which is what BirdDog's own OSD assets are authored at.
func DetectMode() Mode {
	def := Mode{Width: 1920, Height: 1080}
	out, err := exec.Command("cat", "/sys/kernel/debug/dri/0/summary").Output()
	if err != nil {
		return def
	}
	// Line looks like:  "    Display mode: 1920x1080p60"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Display mode:") {
			continue
		}
		spec := strings.TrimSpace(strings.TrimPrefix(line, "Display mode:"))
		var w, h int
		if _, err := fmt.Sscanf(spec, "%dx%d", &w, &h); err == nil && w > 0 && h > 0 {
			return Mode{Width: w, Height: h}
		}
	}
	return def
}
