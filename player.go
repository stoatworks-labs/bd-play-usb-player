package main

// The player: one GStreamer process per item, supervised.
//
// Why a process per item rather than one long-lived pipeline with playbin's
// about-to-finish gapless handoff: on this device the MPP decoder and the KMS
// sink both hold hardware state that is only reliably reset by teardown. A
// single pipeline that switches between a 4K H.264 clip, a PNG, and a 720p
// clip has to renegotiate caps across the DRM plane mid-flight, and the stock
// 2020-vintage rockchipmpp plugin does not do that gracefully. Restarting is
// ~300 ms of black between items, which for signage is an acceptable price for
// not wedging the VOP.

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	gstBin = "gst-launch-1.0"
	// hdmiAudioDevice is card 1 on the PLAY: "rockchiphdmi". Card 0 is the
	// SoC's own i2s, which goes nowhere on this hardware.
	hdmiAudioDevice = "hw:1,0"
)

// PlayerState is the externally visible state machine.
type PlayerState string

const (
	StateIdle    PlayerState = "idle"
	StatePlaying PlayerState = "playing"
	StateError   PlayerState = "error"
)

// Options configure a play session.
type Options struct {
	Order      Order
	Loop       bool
	ImageDwell time.Duration // how long a still stays up
	Mute       bool
}

// DefaultImageDwell is how long a still image or PDF page holds the screen.
const DefaultImageDwell = 10 * time.Second

// Player runs a playlist on the display.
type Player struct {
	mu       sync.Mutex
	state    PlayerState
	current  Item
	lastErr  string
	playlist *Playlist
	opts     Options
	mode     Mode

	cmd    *exec.Cmd
	stopCh chan struct{}
	doneCh chan struct{}

	display *Display
	pdf     *PDFRenderer
	log     *Logger
}

func NewPlayer(display *Display, pdf *PDFRenderer, log *Logger) *Player {
	return &Player{
		state:   StateIdle,
		display: display,
		pdf:     pdf,
		log:     log,
	}
}

// Start begins playing a set of items. Any current session is stopped first.
func (p *Player) Start(items []Item, opts Options) error {
	if len(items) == 0 {
		return fmt.Errorf("nothing to play")
	}
	p.Stop()

	if opts.ImageDwell <= 0 {
		opts.ImageDwell = DefaultImageDwell
	}

	// Take the display before we promise the caller anything, so a failure to
	// stop PPApp surfaces as a failed request rather than a silently dead
	// playlist.
	if err := p.display.Acquire(); err != nil {
		p.mu.Lock()
		p.state = StateError
		p.lastErr = err.Error()
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	p.mode = DetectMode()
	p.playlist = NewPlaylist(items, opts.Order, opts.Loop, time.Now().UnixNano())
	p.opts = opts
	p.state = StatePlaying
	p.lastErr = ""
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	stop, done, pl := p.stopCh, p.doneCh, p.playlist
	p.mu.Unlock()

	p.log.Info("play: %d item(s), order=%s loop=%v dwell=%s mode=%dx%d",
		len(items), opts.Order, opts.Loop, opts.ImageDwell, p.mode.Width, p.mode.Height)

	go p.run(pl, stop, done)
	return nil
}

// run is the playback loop. It owns the display until it returns.
func (p *Player) run(pl *Playlist, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	defer p.display.Release()

	// consecutiveFailures guards against a playlist of entirely undecodable
	// files spinning the CPU in a tight restart loop. A stick full of
	// corrupt MP4s should settle into an error state, not a fork bomb.
	consecutiveFailures := 0

	for {
		select {
		case <-stop:
			return
		default:
		}

		item, ok := pl.Next()
		if !ok {
			p.log.Info("playlist finished")
			p.mu.Lock()
			p.state = StateIdle
			p.current = Item{}
			p.mu.Unlock()
			return
		}

		p.mu.Lock()
		p.current = item
		p.mu.Unlock()

		err := p.playItem(item, stop)
		switch {
		case err == nil:
			consecutiveFailures = 0
		case isStopped(err):
			return
		default:
			consecutiveFailures++
			p.log.Warn("play %s: %v", item.Rel, err)
			p.mu.Lock()
			p.lastErr = fmt.Sprintf("%s: %v", item.Name, err)
			p.mu.Unlock()
			if consecutiveFailures >= maxInt(3, pl.Len()) {
				p.log.Error("giving up: %d consecutive failures", consecutiveFailures)
				p.mu.Lock()
				p.state = StateError
				p.mu.Unlock()
				return
			}
			// Brief pause so a failing playlist does not spin.
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

// errStopped signals a deliberate stop rather than a playback failure.
type errStopped struct{}

func (errStopped) Error() string { return "stopped" }
func isStopped(err error) bool   { _, ok := err.(errStopped); return ok }

// playItem runs one item to completion.
func (p *Player) playItem(item Item, stop <-chan struct{}) error {
	switch item.Kind {
	case KindVideo:
		return p.runPipeline(p.videoPipeline(item.Path), 0, stop)
	case KindImage:
		return p.runPipeline(p.imagePipeline(item.Path), p.opts.ImageDwell, stop)
	case KindPDF:
		return p.playPDF(item, stop)
	}
	return fmt.Errorf("unsupported kind %q", item.Kind)
}

// playPDF renders the document to page images (cached) and shows each in turn.
func (p *Player) playPDF(item Item, stop <-chan struct{}) error {
	if p.pdf == nil || !p.pdf.Available() {
		return fmt.Errorf("PDF rendering not available")
	}
	pages, err := p.pdf.Render(item.Path, p.mode)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if len(pages) == 0 {
		return fmt.Errorf("no pages")
	}
	for _, page := range pages {
		select {
		case <-stop:
			return errStopped{}
		default:
		}
		if err := p.runPipeline(p.imagePipeline(page), p.opts.ImageDwell, stop); err != nil {
			return err
		}
	}
	return nil
}

// videoPipeline builds the hardware-decode pipeline.
//
// decodebin picks mppvideodec for the codecs MPP handles and falls back to
// software for the rest, so one pipeline covers both without us having to
// probe the file first. Audio goes to the HDMI card; a clip with no audio
// track simply never links that branch, which decodebin handles by itself.
func (p *Player) videoPipeline(path string) []string {
	audio := fmt.Sprintf("alsasink device=%s", hdmiAudioDevice)
	if p.opts.Mute {
		audio = "fakesink"
	}
	return []string{
		"playbin",
		"uri=" + fileURI(path),
		"video-sink=kmssink",
		"audio-sink=" + audio,
		// Text (subtitle) rendering would need a font stack and a compositor
		// we do not have; disabling it stops playbin from trying.
		"flags=0x00000003", // GST_PLAY_FLAG_VIDEO | GST_PLAY_FLAG_AUDIO
	}
}

// imagePipeline shows a still, scaled and letterboxed to the panel.
//
// imagefreeze turns the single decoded frame into a live stream, which kmssink
// needs — handing it one buffer and EOS leaves the plane briefly set and then
// torn down, which flashes rather than displays. add-borders keeps the aspect
// ratio instead of stretching, which matters for slides.
func (p *Player) imagePipeline(path string) []string {
	caps := fmt.Sprintf("video/x-raw,format=NV12,width=%d,height=%d,framerate=10/1", p.mode.Width, p.mode.Height)
	return []string{
		"filesrc", "location=" + path,
		"!", "decodebin",
		"!", "videoconvert",
		"!", "videoscale", "add-borders=true",
		"!", "imagefreeze",
		"!", caps,
		"!", "kmssink",
	}
}

// runPipeline spawns gst-launch and waits. If dwell is non-zero the pipeline is
// stopped after that long (stills run forever otherwise); if zero it runs to
// EOS (video).
func (p *Player) runPipeline(args []string, dwell time.Duration, stop <-chan struct{}) error {
	cmd := exec.Command(gstBin, append([]string{"-q"}, args...)...)
	// Put the child in its own process group so we can signal the whole group.
	// gst-launch spawns helper threads, not children, but the group also means
	// a stuck pipeline cannot outlive us attached to our terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", gstBin, err)
	}

	p.mu.Lock()
	p.cmd = cmd
	p.mu.Unlock()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var dwellCh <-chan time.Time
	if dwell > 0 {
		t := time.NewTimer(dwell)
		defer t.Stop()
		dwellCh = t.C
	}

	select {
	case <-stop:
		terminate(cmd)
		<-waitCh
		return errStopped{}

	case <-dwellCh:
		// Normal end of a still: we asked for it, so this is success.
		terminate(cmd)
		<-waitCh
		return nil

	case err := <-waitCh:
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			// gst-launch is noisy on success too (mpp_info banners), so only
			// surface stderr when it actually failed.
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("%s", lastLines(msg, 3))
		}
		return nil
	}
}

// terminate stops a pipeline politely, then firmly.
//
// SIGINT first because gst-launch handles it by sending EOS and shutting the
// pipeline down cleanly, which releases the DRM plane. SIGKILL alone leaves the
// plane configured and the next kmssink fails to set a mode.
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGINT)

	done := make(chan struct{})
	go func() {
		// Poll rather than Wait: the caller owns Wait.
		for i := 0; i < 30; i++ {
			if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
				close(done)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

// Stop ends the current session and returns the display to BirdDog.
func (p *Player) Stop() {
	p.mu.Lock()
	stop, done := p.stopCh, p.doneCh
	p.stopCh, p.doneCh = nil, nil
	p.mu.Unlock()

	if stop == nil {
		return
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		p.log.Error("playback did not stop within 15s")
	}

	p.mu.Lock()
	p.state = StateIdle
	p.current = Item{}
	p.mu.Unlock()
}

// Next skips to the next item.
func (p *Player) Next() {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd != nil {
		terminate(cmd) // the loop will advance on its own
	}
}

// Prev goes back one item.
func (p *Player) Prev() {
	p.mu.Lock()
	pl, cmd := p.playlist, p.cmd
	p.mu.Unlock()
	if pl != nil {
		pl.Prev()
	}
	if cmd != nil {
		terminate(cmd)
	}
}

// Status is the player's externally visible state.
type Status struct {
	State    PlayerState `json:"state"`
	Current  Item        `json:"current"`
	Position int         `json:"position"`
	Total    int         `json:"total"`
	Cycles   int         `json:"cycles"`
	Order    Order       `json:"order"`
	Loop     bool        `json:"loop"`
	Mute     bool        `json:"mute"`
	DwellSec int         `json:"dwell_sec"`
	Error    string      `json:"error,omitempty"`
	Width    int         `json:"width"`
	Height   int         `json:"height"`
}

func (p *Player) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{
		State:    p.state,
		Current:  p.current,
		Order:    p.opts.Order,
		Loop:     p.opts.Loop,
		Mute:     p.opts.Mute,
		DwellSec: int(p.opts.ImageDwell / time.Second),
		Error:    p.lastErr,
		Width:    p.mode.Width,
		Height:   p.mode.Height,
	}
	if p.playlist != nil {
		st.Position, st.Total, st.Cycles = p.playlist.Position()
	}
	return st
}

// SetOrder and SetLoop adjust a running session without restarting it.
func (p *Player) SetOrder(o Order) {
	p.mu.Lock()
	p.opts.Order = o
	pl := p.playlist
	p.mu.Unlock()
	if pl != nil {
		pl.SetOrder(o)
	}
}

func (p *Player) SetLoop(loop bool) {
	p.mu.Lock()
	p.opts.Loop = loop
	pl := p.playlist
	p.mu.Unlock()
	if pl != nil {
		pl.SetLoop(loop)
	}
}

func fileURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// GStreamer wants percent-encoding for the characters that would otherwise
	// terminate or reinterpret the URI. Filenames on a stick routinely contain
	// spaces, #, and ?.
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(abs); i++ {
		c := abs[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.IndexByte("/-_.~", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
