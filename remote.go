package main

// The remote-control session: the PLAY as a CEC playback device with an
// on-screen file browser, driven from the TV's remote.
//
// Life of a session:
//
//   enter    pin the display (PPApp stops), start the browser pipeline, open
//            /dev/cec0 and claim a Playback address, tell the TV we are the
//            active source
//   browse   keys move a cursor over the stick's folders; OK plays a file or
//            a whole folder
//   play     the browser pipeline is stopped, the ordinary player runs; Stop
//            or the end of the playlist brings the browser back
//   leave    close CEC, put the decoder's previous source back, unpin the
//            display (PPApp restarts and its own CEC menu takes over)
//
// One goroutine owns all of the state; keys from CEC and from the REST API
// are posted to it, as are "playback finished" and "stick changed" events.
// That is what keeps the display handover free of races: nothing touches the
// screen or the player except the loop, in order.

import (
	"fmt"
	"sync"
	"time"
)

// remoteDisplay, remotePlayer and remoteScreen are the pieces the session
// drives, as interfaces so the state machine is tested without a device.
type remoteDisplay interface {
	Hold() error
	Unhold()
}

type remotePlayer interface {
	Start(items []Item, opts Options) error
	Stop()
	Next()
	Prev()
	SetOnDone(func())
	SetOrder(Order)
	SetLoop(bool)
	Status() Status
}

type remoteScreen interface {
	Start(mode Mode, first []byte) error
	Show(frame []byte) error
	Stop()
	Kind() string
}

// RemoteConfig is the session's knobs.
type RemoteConfig struct {
	CECPath        string
	OSDName        string // what the TV lists us as (14 characters max)
	AnnounceActive bool   // broadcast <Active Source> on entry
	Dwell          time.Duration
	Etc            string // where the decoder's selection files live
}

type remoteState string

const (
	remoteIdle     remoteState = "idle"
	remoteBrowsing remoteState = "browsing"
	remotePlaying  remoteState = "playing"
)

type remoteEventKind int

const (
	evPlaybackDone remoteEventKind = iota
	evLibrary
	evPlayRequest
)

type remoteEvent struct {
	kind remoteEventKind
	gen  int // playback generation the event belongs to
	// evPlayRequest: what the web UI asked for
	items []Item
	opts  Options
	label string
}

// holdStill is the dwell for a single still picked from the browser: it
// stays until a key says otherwise.
const holdStill = 365 * 24 * time.Hour

// Remote is the session.
type Remote struct {
	cfg     RemoteConfig
	display remoteDisplay
	player  remotePlayer
	screen  remoteScreen
	library func() (*Library, string)
	volume  func() (Volume, bool)
	mode    func() Mode
	openCEC func(path string, log *Logger) (cecAdapter, error)
	guard   *sourceGuard     // may be nil
	trigger *triggerListener // may be nil
	log     *Logger

	events chan remoteEvent
	keys   chan Key

	mu      sync.Mutex
	active  bool
	state   remoteState
	reason  string
	since   time.Time
	stop    chan struct{}
	done    chan struct{}
	cecErr  string
	cecLA   int
	cecPA   string
	playing string
	lastErr string
	dir     string

	// loop-owned
	menu     *Menu
	renderer *Renderer
	gen      int
	cec      cecAdapter
	follower *cecFollower
	cecStop  chan struct{}
	cecDone  chan struct{}
	shuffle  bool
	loop     bool
	stickMsg bool
}

func NewRemote(cfg RemoteConfig, display remoteDisplay, player remotePlayer, screen remoteScreen,
	library func() (*Library, string), volume func() (Volume, bool), mode func() Mode, log *Logger) *Remote {
	if cfg.Dwell <= 0 {
		cfg.Dwell = DefaultImageDwell
	}
	if cfg.OSDName == "" {
		cfg.OSDName = "USB Player"
	}
	return &Remote{
		cfg: cfg, display: display, player: player, screen: screen,
		library: library, volume: volume, mode: mode, openCEC: openCEC, log: log,
		events: make(chan remoteEvent, 8),
		keys:   make(chan Key, 32),
		state:  remoteIdle,
		loop:   true,
	}
}

// RemoteStatus is what the API reports.
type RemoteStatus struct {
	Active   bool   `json:"active"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Since    string `json:"since,omitempty"`
	Dir      string `json:"dir"`
	Playing  string `json:"playing,omitempty"`
	CECError string `json:"cec_error,omitempty"`
	CECAddr  int    `json:"cec_logical_address"`
	CECPhys  string `json:"cec_physical_address,omitempty"`
	Screen   string `json:"screen"`
	Error    string `json:"error,omitempty"`
}

func (r *Remote) Status() RemoteStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := RemoteStatus{
		Active: r.active, State: string(r.state), Reason: r.reason, Dir: r.dir,
		Playing: r.playing, CECError: r.cecErr, CECAddr: r.cecLA, CECPhys: r.cecPA,
		Screen: r.screen.Kind(), Error: r.lastErr,
	}
	if r.active {
		st.Since = r.since.Format(time.RFC3339)
	}
	return st
}

// Active reports whether a session is running.
func (r *Remote) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Enter starts a session. Idempotent while one is running.
func (r *Remote) Enter(reason string) error {
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return nil
	}
	r.active = true
	r.state = remoteBrowsing
	r.reason = reason
	r.since = time.Now()
	r.lastErr, r.cecErr, r.playing = "", "", ""
	r.cecLA, r.cecPA = -1, ""
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	stop, done := r.stop, r.done
	r.mu.Unlock()

	r.log.Info("remote: entering (%s)", reason)
	if r.trigger != nil {
		r.trigger.Suspend()
	}

	if err := r.display.Hold(); err != nil {
		r.abandon(done)
		return fmt.Errorf("take display: %w", err)
	}
	// Whatever the web UI had going stops; the display stays ours.
	r.player.SetOnDone(nil)
	r.player.Stop()

	m := r.mode()
	rend, err := NewRenderer(m.Width, m.Height)
	if err != nil {
		r.display.Unhold()
		r.abandon(done)
		return fmt.Errorf("renderer: %w", err)
	}
	r.renderer = rend
	lib, _ := r.library()
	r.menu = NewMenu(lib, rend.Rows())
	r.menu.Shuffle, r.menu.Loop = r.shuffle, r.loop
	r.mu.Lock()
	r.stickMsg = lib == nil
	r.mu.Unlock()

	// Drain events left over from an earlier session.
	for len(r.events) > 0 {
		<-r.events
	}
	for len(r.keys) > 0 {
		<-r.keys
	}

	if err := r.screen.Start(m, r.frame()); err != nil {
		r.display.Unhold()
		r.abandon(done)
		return fmt.Errorf("on-screen browser: %w", err)
	}
	r.log.Info("remote: browser on screen at %dx%d via the %s pipeline", m.Width, m.Height, r.screen.Kind())

	r.openRemote()
	go r.run(stop, done)
	return nil
}

// abandon unwinds a failed Enter.
func (r *Remote) abandon(done chan struct{}) {
	r.mu.Lock()
	r.active = false
	r.state = remoteIdle
	r.mu.Unlock()
	if r.trigger != nil {
		r.trigger.Resume()
	}
	close(done)
}

// openRemote opens the CEC adapter; failure is reported on screen and in the
// status but is not fatal — keys still arrive from the REST API.
func (r *Remote) openRemote() {
	if r.cfg.CECPath == "" {
		r.setCECError("CEC disabled")
		return
	}
	dev, err := r.openCEC(r.cfg.CECPath, r.log)
	if err != nil {
		r.log.Warn("remote: %v", err)
		r.setCECError(err.Error())
		return
	}
	if err := dev.Claim(r.cfg.OSDName, 4*time.Second); err != nil {
		r.log.Warn("remote: claim: %v", err)
		dev.Close()
		r.setCECError(err.Error())
		return
	}
	r.cec = dev
	r.follower = newCECFollower(dev, r.log, r.pushKey, r.deckStatus)
	r.cecStop = make(chan struct{})
	r.cecDone = make(chan struct{})
	go func() {
		defer close(r.cecDone)
		dev.Run(r.cecStop, r.follower.handle)
	}()
	r.mu.Lock()
	r.cecErr = ""
	r.cecLA = int(dev.LogAddr())
	r.cecPA = physAddrString(dev.PhysAddr())
	r.mu.Unlock()
	if r.cfg.AnnounceActive {
		r.follower.announce()
	}
}

func (r *Remote) setCECError(s string) {
	r.mu.Lock()
	r.cecErr = s
	r.mu.Unlock()
}

// closeRemote releases the adapter. Must happen before PPApp restarts.
func (r *Remote) closeRemote() {
	if r.cec == nil {
		return
	}
	close(r.cecStop)
	select {
	case <-r.cecDone:
	case <-time.After(2 * time.Second):
		r.log.Warn("remote: CEC loop did not stop in time")
	}
	if err := r.cec.Close(); err != nil {
		r.log.Warn("remote: close CEC: %v", err)
	}
	r.cec, r.follower = nil, nil
}

// pushKey delivers a key to the loop; used by the CEC follower and the API.
func (r *Remote) pushKey(k Key) {
	select {
	case r.keys <- k:
	default:
		r.log.Warn("remote: dropping key %s (queue full)", k)
	}
}

// Key injects a remote key, as if the TV had sent it.
func (r *Remote) Key(k Key) error {
	if !r.Active() {
		return fmt.Errorf("remote session not active")
	}
	r.pushKey(k)
	return nil
}

// Refresh tells the session the stick changed.
func (r *Remote) Refresh() {
	select {
	case r.events <- remoteEvent{kind: evLibrary}:
	default:
	}
}

// PlayExternal plays a web-UI selection inside the session: the browser
// comes back when it finishes. Fails if no session is running.
func (r *Remote) PlayExternal(items []Item, opts Options) error {
	if !r.Active() {
		return fmt.Errorf("remote session not active")
	}
	if len(items) == 0 {
		return fmt.Errorf("nothing to play")
	}
	label := "the web UI selection"
	if len(items) == 1 {
		label = items[0].Name
	}
	select {
	case r.events <- remoteEvent{kind: evPlayRequest, items: items, opts: opts, label: label}:
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("remote session busy")
	}
}

// Exit ends the session and waits for the display to be handed back.
func (r *Remote) Exit() {
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	stop, done := r.stop, r.done
	r.mu.Unlock()
	select {
	case <-stop:
	default:
		close(stop)
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		r.log.Error("remote: session did not finish within 60s")
	}
}

func (r *Remote) deckStatus() uint8 {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.state == remotePlaying:
		return cecOpDeckInfoPlay
	case r.stickMsg:
		return cecOpDeckInfoNoMedia
	default:
		return cecOpDeckInfoStop
	}
}

// run is the session loop.
func (r *Remote) run(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-stop:
			r.finish()
			return
		case k := <-r.keys:
			if r.handleKey(k) {
				r.finish()
				return
			}
		case ev := <-r.events:
			switch ev.kind {
			case evPlaybackDone:
				if r.stateIs(remotePlaying) && ev.gen == r.gen {
					r.log.Info("remote: playback finished; back to the browser")
					r.backToBrowser("")
				}
			case evLibrary:
				lib, _ := r.library()
				r.menu.Reload(lib)
				r.mu.Lock()
				r.stickMsg = lib == nil
				r.mu.Unlock()
				if r.stateIs(remoteBrowsing) {
					r.render()
				}
			case evPlayRequest:
				if r.stateIs(remotePlaying) {
					r.stopPlayback()
				}
				r.startPlayback(ev.items, ev.opts, ev.label)
			}
		}
	}
}

func (r *Remote) stateIs(s remoteState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == s
}

// handleKey acts on one key. Returns true when the session should end.
func (r *Remote) handleKey(k Key) bool {
	if r.stateIs(remotePlaying) {
		return r.handlePlayingKey(k)
	}
	return r.handleBrowsingKey(k)
}

func (r *Remote) handleBrowsingKey(k Key) bool {
	m := r.menu
	switch k {
	case KeyUp:
		m.Up()
	case KeyDown:
		m.Down()
	case KeyPageUp:
		m.PageUp()
	case KeyPageDown:
		m.PageDown()
	case KeySelect, KeyRight:
		e, ok := m.Selected()
		if !ok {
			return false
		}
		if m.Enter() {
			break
		}
		if sel, ok := m.SelectionFor(e); ok {
			r.play(sel)
			return false
		}
		if k == KeyRight {
			return false
		}
		r.setError("That item cannot be played")
	case KeyLeft:
		m.Back()
	case KeyBack:
		if !m.Back() {
			return true // Exit at the root: back to BirdDog
		}
	case KeyPlay, KeyGreen:
		if sel, ok := m.FolderSelection(); ok {
			r.play(sel)
			return false
		}
		r.setError("Nothing to play in this folder")
	case KeyStop, KeyRed:
		return true
	case KeyYellow:
		r.loop = !r.loop
		m.Loop = r.loop
	case KeyBlue:
		r.shuffle = !r.shuffle
		m.Shuffle = r.shuffle
	case KeyInfo, KeyNext, KeyPrev, KeyPause, KeyEject:
		// Nothing to do in the list.
	}
	r.render()
	return false
}

func (r *Remote) handlePlayingKey(k Key) bool {
	switch k {
	case KeyStop, KeyBack:
		r.stopPlayback()
		r.backToBrowser("")
	case KeyRed:
		r.stopPlayback()
		return true
	case KeyNext, KeyRight:
		r.player.Next()
	case KeyPrev, KeyLeft:
		r.player.Prev()
	case KeySelect, KeyPlay:
		// On a held still, OK moves on; for a playlist it is a no-op.
		if r.player.Status().DwellSec >= int(holdStill/time.Second) {
			r.player.Next()
		}
	case KeyYellow:
		r.loop = !r.loop
		r.menu.Loop = r.loop
		r.player.SetLoop(r.loop)
	case KeyBlue:
		r.shuffle = !r.shuffle
		r.menu.Shuffle = r.shuffle
		if r.shuffle {
			r.player.SetOrder(OrderRandom)
		} else {
			r.player.SetOrder(OrderSequential)
		}
	case KeyPause:
		r.log.Info("remote: pause is not supported (the playback pipeline cannot be paused)")
	}
	return false
}

// play turns a browser selection into playback options.
func (r *Remote) play(sel MenuSelection) {
	opts := Options{Order: OrderSequential, Loop: r.loop && !sel.Single, ImageDwell: r.cfg.Dwell}
	if r.shuffle {
		opts.Order = OrderRandom
	}
	if sel.Single {
		opts.ImageDwell = holdStill
	}
	r.startPlayback(sel.Items, opts, sel.Label)
}

// startPlayback hands the display to the player.
func (r *Remote) startPlayback(items []Item, opts Options, label string) {
	r.screen.Stop()
	r.gen++
	gen := r.gen
	r.player.SetOnDone(func() {
		select {
		case r.events <- remoteEvent{kind: evPlaybackDone, gen: gen}:
		default:
		}
	})
	if err := r.player.Start(items, opts); err != nil {
		r.log.Warn("remote: play %s: %v", label, err)
		r.setError(fmt.Sprintf("Could not play: %v", err))
		r.backToBrowser("")
		return
	}
	r.mu.Lock()
	r.state = remotePlaying
	r.playing = label
	r.lastErr = ""
	r.mu.Unlock()
	r.log.Info("remote: playing %s (%d item(s))", label, len(items))
}

func (r *Remote) stopPlayback() {
	r.gen++ // any finish event from the session being stopped is stale
	r.player.SetOnDone(nil)
	r.player.Stop()
}

// backToBrowser restarts the browser pipeline after playback.
func (r *Remote) backToBrowser(status string) {
	r.mu.Lock()
	r.state = remoteBrowsing
	r.playing = ""
	if status != "" {
		r.lastErr = status
	}
	r.mu.Unlock()
	if err := r.screen.Start(r.mode(), r.frame()); err != nil {
		r.log.Error("remote: browser pipeline: %v", err)
	}
}

func (r *Remote) setError(s string) {
	r.mu.Lock()
	r.lastErr = s
	r.mu.Unlock()
}

// render redraws the browser if it is on screen.
func (r *Remote) render() {
	if err := r.screen.Show(r.frame()); err != nil {
		r.log.Warn("remote: show: %v", err)
	}
}

// frame builds the view and draws it.
func (r *Remote) frame() []byte {
	return r.renderer.Draw(r.view())
}

func (r *Remote) view() osdView {
	r.mu.Lock()
	cecErr, lastErr := r.cecErr, r.lastErr
	la := r.cecLA
	r.mu.Unlock()

	v := osdView{Title: "USB Player"}
	if vol, ok := r.volume(); ok {
		label := vol.Label
		if label == "" {
			label = vol.Device
		}
		v.Subtitle = fmt.Sprintf("%s (%s)", label, vol.FSType)
	}

	lib, libErr := r.library()
	if lib == nil {
		v.Message = []string{"No USB storage found",
			"Insert a stick with videos, pictures or PDFs.",
			"Press Exit or Stop to return to BirdDog."}
		if libErr != "" {
			v.Message[1] = libErr
		}
	} else {
		v.Breadcrumb = r.menu.Breadcrumb()
		v.Entries, v.First = r.menu.Visible()
		v.Cursor = r.menu.Cursor()
		v.Total = len(r.menu.Entries())
		r.mu.Lock()
		r.dir = r.menu.Dir()
		r.mu.Unlock()
		if len(v.Entries) == 0 {
			v.Message = []string{"Nothing playable here", "Press Exit to go back."}
		}
	}

	switch {
	case lastErr != "":
		v.Status, v.StatusBad = lastErr, true
	case cecErr != "":
		v.Status, v.StatusBad = "TV remote not available ("+cecErr+") — use http://"+hostname()+":8091/", true
	default:
		v.Status = fmt.Sprintf("TV remote ready (CEC address %d)  ·  shuffle %s  ·  loop %s",
			la, onOff(r.shuffle), onOff(r.loop))
	}

	v.Legend = []legendItem{
		{Text: "Up/Down choose"},
		{Text: "OK open or play"},
		{Text: "Exit back"},
		{Text: "Play this folder"},
		{Text: "Stop leave"},
		{Color: colKeyRed, Text: "BirdDog"},
		{Color: colKeyBlue, Text: "shuffle"},
		{Color: colKeyYellow, Text: "loop"},
	}
	return v
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// finish tears the session down, in the order that keeps the display safe.
func (r *Remote) finish() {
	r.log.Info("remote: leaving")
	if r.stateIs(remotePlaying) {
		r.stopPlayback()
	}
	r.screen.Stop()
	r.closeRemote()
	if r.guard != nil {
		if what, err := r.guard.Restore(); err != nil {
			r.log.Error("remote: %v — the decoder may dial the USB entry again on restart", err)
		} else {
			r.log.Info("remote: %s", what)
		}
	}
	r.mu.Lock()
	r.active = false
	r.state = remoteIdle
	r.playing = ""
	r.mu.Unlock()
	r.display.Unhold()
	if r.trigger != nil {
		r.trigger.Resume()
	}
	r.log.Info("remote: left; display returned to BirdDog")
}
