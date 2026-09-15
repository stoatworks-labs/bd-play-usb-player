package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// The session's collaborators, faked. They record calls and let a test end
// a "playback" the way the real player does: by calling the OnDone hook.

type fakeDisplay struct {
	mu      sync.Mutex
	holds   int
	unholds int
	failNow bool
}

func (d *fakeDisplay) Hold() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failNow {
		return errors.New("PPApp still running")
	}
	d.holds++
	return nil
}

func (d *fakeDisplay) Unhold() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unholds++
}

func (d *fakeDisplay) counts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.holds, d.unholds
}

type fakePlayer struct {
	mu      sync.Mutex
	starts  int
	stops   int
	nexts   int
	prevs   int
	items   []Item
	opts    Options
	playing bool
	onDone  func()
	failing bool
}

func (p *fakePlayer) Start(items []Item, opts Options) error {
	p.Stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failing {
		return errors.New("gst-launch exploded")
	}
	p.starts++
	p.items, p.opts, p.playing = items, opts, true
	return nil
}

// Stop mirrors the real player: the OnDone hook fires before Stop returns.
func (p *fakePlayer) Stop() {
	p.mu.Lock()
	if !p.playing {
		p.mu.Unlock()
		return
	}
	p.playing = false
	p.stops++
	fn := p.onDone
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// end is the playlist finishing on its own.
func (p *fakePlayer) end() {
	p.mu.Lock()
	p.playing = false
	fn := p.onDone
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (p *fakePlayer) Next() { p.mu.Lock(); p.nexts++; p.mu.Unlock() }
func (p *fakePlayer) Prev() { p.mu.Lock(); p.prevs++; p.mu.Unlock() }
func (p *fakePlayer) SetOnDone(fn func()) {
	p.mu.Lock()
	p.onDone = fn
	p.mu.Unlock()
}
func (p *fakePlayer) SetOrder(o Order) { p.mu.Lock(); p.opts.Order = o; p.mu.Unlock() }
func (p *fakePlayer) SetLoop(l bool)   { p.mu.Lock(); p.opts.Loop = l; p.mu.Unlock() }
func (p *fakePlayer) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{State: StateIdle, DwellSec: int(p.opts.ImageDwell / time.Second)}
	if p.playing {
		st.State = StatePlaying
	}
	return st
}

func (p *fakePlayer) snapshot() (starts, stops int, items []Item, opts Options, playing bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts, p.stops, p.items, p.opts, p.playing
}

type fakeScreen struct {
	mu      sync.Mutex
	starts  int
	stops   int
	shows   int
	running bool
	frame   int
}

func (s *fakeScreen) Start(mode Mode, first []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	s.running = true
	s.starts++
	s.frame = len(first)
	return nil
}

func (s *fakeScreen) Show(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return errors.New("not running")
	}
	s.shows++
	s.frame = len(frame)
	return nil
}

func (s *fakeScreen) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.running = false
		s.stops++
	}
}

func (s *fakeScreen) Kind() string { return "fake" }

func (s *fakeScreen) snapshot() (starts, stops, shows int, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, s.stops, s.shows, s.running
}

type harness struct {
	r       *Remote
	display *fakeDisplay
	player  *fakePlayer
	screen  *fakeScreen
	cec     *fakeCEC
	lib     *Library
	mu      sync.Mutex
}

func newHarness(t *testing.T, lib *Library) *harness {
	t.Helper()
	h := &harness{display: &fakeDisplay{}, player: &fakePlayer{}, screen: &fakeScreen{}, cec: newFakeCEC(), lib: lib}
	h.r = NewRemote(RemoteConfig{CECPath: "/dev/cec-fake", OSDName: "USB Player", AnnounceActive: true},
		h.display, h.player, h.screen,
		func() (*Library, string) { h.mu.Lock(); defer h.mu.Unlock(); return h.lib, "" },
		func() (Volume, bool) {
			return Volume{Label: "STICK", FSType: "vfat", Device: "/dev/sda1"}, h.lib != nil
		},
		func() Mode { return Mode{Width: 320, Height: 180} },
		NewLogger())
	h.r.openCEC = func(path string, log *Logger) (cecAdapter, error) { return h.cec, nil }
	return h
}

func (h *harness) setLibrary(lib *Library) {
	h.mu.Lock()
	h.lib = lib
	h.mu.Unlock()
}

// press delivers a key over the fake CEC bus, as the TV would.
func (h *harness) press(t *testing.T, cmd uint8) {
	t.Helper()
	h.cec.inject(t, newCECMsg(0, 4, cecMsgUserControlPressed, cmd))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRemoteEnterBrowseSelectAndFinish(t *testing.T) {
	h := newHarness(t, menuLibrary(t))
	if err := h.r.Enter("test"); err != nil {
		t.Fatal(err)
	}
	if st := h.r.Status(); !st.Active || st.State != "browsing" || st.CECAddr != 4 || st.CECError != "" {
		t.Fatalf("status after enter: %+v", st)
	}
	if holds, _ := h.display.counts(); holds != 1 {
		t.Fatal("display not held")
	}
	if starts, _, _, running := h.screen.snapshot(); starts != 1 || !running {
		t.Fatal("browser pipeline not started")
	}
	if h.cec.name != "USB Player" || h.cec.claims != 1 {
		t.Errorf("CEC claim: name %q, claims %d", h.cec.name, h.cec.claims)
	}
	waitFor(t, "Active Source announce", func() bool {
		for _, m := range h.cec.transmitted() {
			if m.Msg[1] == cecMsgActiveSource {
				return true
			}
		}
		return false
	})

	// Down, Down lands on clip1.mp4 (row 0 is play-all, row 1 photos/).
	h.press(t, cecUICmdDown)
	h.press(t, cecUICmdDown)
	waitFor(t, "two redraws", func() bool { _, _, shows, _ := h.screen.snapshot(); return shows >= 2 })
	h.press(t, cecUICmdSelect)
	waitFor(t, "playback to start", func() bool { starts, _, _, _, _ := h.player.snapshot(); return starts == 1 })
	_, _, items, opts, _ := h.player.snapshot()
	if len(items) != 1 || items[0].Name != "clip1.mp4" {
		t.Fatalf("played %+v", items)
	}
	if opts.ImageDwell != holdStill || opts.Loop {
		t.Errorf("a single file should hold and not loop: %+v", opts)
	}
	if _, stops, _, running := h.screen.snapshot(); stops != 1 || running {
		t.Fatal("browser pipeline must be stopped before the player runs")
	}
	if st := h.r.Status(); st.State != "playing" || st.Playing != "clip1.mp4" {
		t.Errorf("status while playing: %+v", st)
	}

	// The clip ends: the browser comes back by itself.
	h.player.end()
	waitFor(t, "browser to return", func() bool { starts, _, _, running := h.screen.snapshot(); return starts == 2 && running })
	if st := h.r.Status(); st.State != "browsing" {
		t.Errorf("status after playback: %+v", st)
	}

	// Red: back to BirdDog.
	h.press(t, cecUICmdF2Red)
	waitFor(t, "session to end", func() bool { return !h.r.Active() })
	if _, unholds := h.display.counts(); unholds != 1 {
		t.Error("display not returned")
	}
	if !h.cec.closed {
		t.Error("CEC adapter not closed")
	}
	if _, _, _, running := h.screen.snapshot(); running {
		t.Error("browser pipeline still running after exit")
	}
}

func TestRemoteStopDuringPlaybackReturnsToBrowser(t *testing.T) {
	h := newHarness(t, menuLibrary(t))
	if err := h.r.Enter("test"); err != nil {
		t.Fatal(err)
	}
	h.press(t, cecUICmdSelect) // play everything
	waitFor(t, "playback", func() bool { starts, _, _, _, _ := h.player.snapshot(); return starts == 1 })
	_, _, items, opts, _ := h.player.snapshot()
	if len(items) != 6 || !opts.Loop || opts.ImageDwell != DefaultImageDwell {
		t.Fatalf("play-all opts: %d items, %+v", len(items), opts)
	}

	// Transport keys reach the player; a stale finish event must not
	// restart the browser while the next thing plays.
	h.press(t, cecUICmdForward)
	h.press(t, cecUICmdBackward)
	waitFor(t, "next/prev", func() bool {
		h.player.mu.Lock()
		defer h.player.mu.Unlock()
		return h.player.nexts == 1 && h.player.prevs == 1
	})

	h.press(t, cecUICmdStop)
	waitFor(t, "browser back", func() bool { starts, _, _, running := h.screen.snapshot(); return starts == 2 && running })
	if _, stops, _, _, playing := h.player.snapshot(); stops != 1 || playing {
		t.Error("player should have been stopped")
	}
	if !h.r.Active() {
		t.Fatal("Stop while playing should not leave the session")
	}

	// Stop in the browser leaves.
	h.press(t, cecUICmdStop)
	waitFor(t, "session end", func() bool { return !h.r.Active() })
}

func TestRemoteExitWhilePlaying(t *testing.T) {
	h := newHarness(t, menuLibrary(t))
	if err := h.r.Enter("test"); err != nil {
		t.Fatal(err)
	}
	h.press(t, cecUICmdSelect)
	waitFor(t, "playback", func() bool { _, _, _, _, playing := h.player.snapshot(); return playing })
	h.r.Exit()
	if h.r.Active() {
		t.Fatal("still active after Exit")
	}
	_, stops, _, _, playing := h.player.snapshot()
	if playing || stops != 1 {
		t.Error("playback should be stopped on exit")
	}
	if _, unholds := h.display.counts(); unholds != 1 {
		t.Error("display not returned on exit")
	}
	if !h.cec.closed {
		t.Error("CEC not closed on exit")
	}
	// Exit twice is harmless, and a new session can start.
	h.r.Exit()
	if err := h.r.Enter("again"); err != nil {
		t.Fatal(err)
	}
	if holds, _ := h.display.counts(); holds != 2 {
		t.Error("second session did not take the display")
	}
	h.r.Exit()
}

func TestRemoteWebUIPlaybackStaysInTheSession(t *testing.T) {
	h := newHarness(t, menuLibrary(t))
	if err := h.r.Enter("test"); err != nil {
		t.Fatal(err)
	}
	items := h.lib.Select("photos", true)
	if err := h.r.PlayExternal(items, Options{Order: OrderRandom, Loop: true, ImageDwell: 3 * time.Second}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "playback", func() bool { starts, _, _, _, _ := h.player.snapshot(); return starts == 1 })
	_, _, got, opts, _ := h.player.snapshot()
	if len(got) != 3 || opts.Order != OrderRandom || opts.ImageDwell != 3*time.Second {
		t.Errorf("external play: %d items, %+v", len(got), opts)
	}
	if st := h.r.Status(); st.State != "playing" || st.Playing != "the web UI selection" {
		t.Errorf("status: %+v", st)
	}
	h.player.end()
	waitFor(t, "browser back", func() bool { starts, _, _, running := h.screen.snapshot(); return starts == 2 && running })
	if err := h.r.PlayExternal(items, Options{}); err != nil {
		t.Fatal(err)
	}
	h.r.Exit()
	if err := h.r.PlayExternal(items, Options{}); err == nil {
		t.Error("PlayExternal outside a session should fail")
	}
}

func TestRemoteNoStickShowsAMessageUntilOneArrives(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.r.Enter("test"); err != nil {
		t.Fatal(err)
	}
	if h.r.deckStatus() != cecOpDeckInfoNoMedia {
		t.Error("deck status should report no media")
	}
	// Keys that would play do nothing without a stick.
	h.press(t, cecUICmdSelect)
	h.press(t, cecUICmdPlay)
	time.Sleep(30 * time.Millisecond)
	if starts, _, _, _, _ := h.player.snapshot(); starts != 0 {
		t.Error("nothing should play without a stick")
	}
	h.setLibrary(menuLibrary(t))
	h.r.Refresh()
	waitFor(t, "the stick to be noticed", func() bool { return h.r.deckStatus() == cecOpDeckInfoStop })
	h.press(t, cecUICmdSelect)
	waitFor(t, "playback", func() bool { starts, _, _, _, _ := h.player.snapshot(); return starts == 1 })
	if h.r.deckStatus() != cecOpDeckInfoPlay {
		t.Error("deck status should report playing")
	}
	h.r.Exit()
}

func TestRemoteExitAtRootLeavesAndBackDescends(t *testing.T) {
	h := newHarness(t, menuLibrary(t))
	if err := h.r.Enter("test"); err != nil {
		t.Fatal(err)
	}
	h.press(t, cecUICmdDown)
	h.press(t, cecUICmdSelect) // into photos/
	waitFor(t, "descend", func() bool { return h.r.Status().Dir == "photos" })
	h.press(t, cecUICmdExit) // back to the root
	waitFor(t, "ascend", func() bool { return h.r.Status().Dir == "" })
	if !h.r.Active() {
		t.Fatal("Exit inside a folder must not leave the session")
	}
	h.press(t, cecUICmdExit) // at the root: leave
	waitFor(t, "session end", func() bool { return !h.r.Active() })
}

func TestRemoteEnterFailsCleanlyWhenTheDisplayCannotBeTaken(t *testing.T) {
	h := newHarness(t, menuLibrary(t))
	h.display.failNow = true
	if err := h.r.Enter("test"); err == nil {
		t.Fatal("Enter should fail")
	}
	if h.r.Active() {
		t.Error("session must not be active after a failed Enter")
	}
	if starts, _, _, _ := h.screen.snapshot(); starts != 0 {
		t.Error("no pipeline should have started")
	}
	if h.cec.claims != 0 {
		t.Error("CEC must not be touched while PPApp may still own it")
	}
	if err := h.r.Key(KeyDown); err == nil {
		t.Error("keys are refused outside a session")
	}
}

func TestRemoteSurvivesAMissingCECAdapter(t *testing.T) {
	h := newHarness(t, menuLibrary(t))
	h.r.openCEC = func(path string, log *Logger) (cecAdapter, error) { return nil, errors.New("no such device") }
	if err := h.r.Enter("test"); err != nil {
		t.Fatal(err)
	}
	st := h.r.Status()
	if !st.Active || st.CECError == "" {
		t.Fatalf("status: %+v", st)
	}
	// The API still drives it.
	if err := h.r.Key(KeySelect); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "playback from an API key", func() bool { starts, _, _, _, _ := h.player.snapshot(); return starts == 1 })
	h.r.Exit()
}

func TestRemotePlayFailureReturnsToBrowser(t *testing.T) {
	h := newHarness(t, menuLibrary(t))
	h.player.failing = true
	if err := h.r.Enter("test"); err != nil {
		t.Fatal(err)
	}
	h.press(t, cecUICmdSelect)
	waitFor(t, "browser back after the failure", func() bool {
		starts, _, _, running := h.screen.snapshot()
		return starts == 2 && running
	})
	if st := h.r.Status(); st.State != "browsing" || st.Error == "" {
		t.Errorf("status: %+v", st)
	}
	h.r.Exit()
}
