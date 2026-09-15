package main

// Getting into the player from the PLAY's own on-screen menu.
//
// BirdDog's decoder draws a source list on HDMI that a CEC-capable TV remote
// navigates: NDI sources, then the SRT list from /etc/birddog_srt_src.json,
// then CloudConnect. Picking an SRT entry makes PPApp dial the entry's
// address as an SRT caller. So bdplay adds one entry that points at
// 127.0.0.1 on a port nobody listens on with SRT — the first datagram of the
// caller's handshake arriving there IS the user choosing "USB player". No
// SRT is spoken; PPApp is about to be stopped for the display anyway.
//
// The catch is what PPApp remembers. The stock decoder persists the selected
// source in /etc/birddog-source1-name (and the mode in the decode settings),
// so if it came back after the player with the USB entry still selected it
// would dial the trigger again, immediately — a loop. sourceGuard watches the
// selection while the decoder runs, keeps the last value that was not ours,
// and puts it back before the decoder is restarted.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	srtListFile     = "birddog_srt_src.json"
	sourceNameFile  = "birddog-source1-name"
	decSettingsFile = "birddog-dec1-settings.json"

	// DefaultOSDEntryName is the label in the PLAY's SRT source list. No
	// spaces or punctuation: the stock AV Setup page builds element ids and
	// jQuery selectors straight from these names.
	DefaultOSDEntryName = "USB-Player"
	// DefaultTriggerPort is where the decoder's SRT caller is sent. UDP,
	// loopback only.
	DefaultTriggerPort = 9107
)

// osdEntry manages our line in the decoder's SRT source list.
type osdEntry struct {
	etc  string // /etc on the device, a temp dir in tests
	name string
	port int
}

// URI is the entry's connection string, in the exact two-parameter shape the
// stock page parses (it splits on "&" and "=" by position).
func (e osdEntry) URI() string {
	return fmt.Sprintf("127.0.0.1:%d?mode=caller&latency=120", e.port)
}

func (e osdEntry) listPath() string { return filepath.Join(e.etc, srtListFile) }

// srtList is the shape of birddog_srt_src.json. Keys we do not know are kept.
type srtList struct {
	Streams []srtEntry
	extra   map[string]json.RawMessage
}

type srtEntry struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
}

func readSRTList(path string) (srtList, error) {
	var l srtList
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return l, err
	}
	raw := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &raw); err != nil {
			// Not JSON the stock page could read either; start over. The
			// list only feeds the OSD, the selection lives elsewhere.
			return l, nil
		}
	}
	if s, ok := raw["streams"]; ok {
		_ = json.Unmarshal(s, &l.Streams)
		delete(raw, "streams")
	}
	l.extra = raw
	return l, nil
}

func writeSRTList(path string, l srtList) error {
	out := map[string]any{}
	for k, v := range l.extra {
		out[k] = v
	}
	if l.Streams == nil {
		l.Streams = []srtEntry{}
	}
	out["streams"] = l.Streams
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o644)
}

// Register adds (or refreshes) our entry. Returns true if the file changed.
func (e osdEntry) Register() (bool, error) {
	l, err := readSRTList(e.listPath())
	if err != nil {
		return false, err
	}
	want := srtEntry{Name: e.name, URI: e.URI()}
	for i, s := range l.Streams {
		if s.Name == e.name {
			if s.URI == want.URI {
				return false, nil
			}
			l.Streams[i] = want
			return true, writeSRTList(e.listPath(), l)
		}
	}
	l.Streams = append(l.Streams, want)
	return true, writeSRTList(e.listPath(), l)
}

// Unregister removes our entry. Returns true if the file changed.
func (e osdEntry) Unregister() (bool, error) {
	l, err := readSRTList(e.listPath())
	if err != nil {
		return false, err
	}
	kept := l.Streams[:0]
	removed := false
	for _, s := range l.Streams {
		if s.Name == e.name {
			removed = true
			continue
		}
		kept = append(kept, s)
	}
	if !removed {
		return false, nil
	}
	l.Streams = kept
	return true, writeSRTList(e.listPath(), l)
}

// selectionValue is what the decoder writes to birddog-source1-name when our
// entry is chosen: SRT:<name>(<uri>).
func (e osdEntry) selectionValue() string {
	return "SRT:" + e.name + "(" + e.URI() + ")"
}

// isOurSelection recognises the selection file pointing at us, by name —
// the URI part may be rewritten by the stock page.
func (e osdEntry) isOurSelection(v string) bool {
	return strings.HasPrefix(strings.TrimSpace(v), "SRT:"+e.name+"(")
}

// triggerListener fires when anything arrives on the trigger port.
type triggerListener struct {
	port int
	log  *Logger
	fire func()
	// quiet suppresses triggers for a while after Arm(false): PPApp retries
	// its connect a few times before the restored selection takes effect.
	mu       sync.Mutex
	armed    bool
	quietTil time.Time
}

func newTriggerListener(port int, log *Logger, fire func()) *triggerListener {
	return &triggerListener{port: port, log: log, fire: fire, armed: true}
}

// Suspend stops the trigger firing while a session is running.
func (t *triggerListener) Suspend() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.armed = false
}

// Resume re-arms after a session, with a quiet period: the decoder retries
// its connect for a while before the restored selection takes effect.
func (t *triggerListener) Resume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.armed = true
	t.quietTil = time.Now().Add(15 * time.Second)
}

// consider decides whether a datagram fires the trigger.
func (t *triggerListener) consider(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.armed || now.Before(t.quietTil) {
		return false
	}
	// One trigger per burst: the caller retransmits its handshake.
	t.quietTil = now.Add(5 * time.Second)
	return true
}

// Run listens until stop is closed.
func (t *triggerListener) Run(stop <-chan struct{}) error {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: t.port}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("trigger port: %w", err)
	}
	go func() {
		<-stop
		conn.Close()
	}()
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-stop:
				return nil
			default:
			}
			return err
		}
		if !from.IP.IsLoopback() {
			continue
		}
		if t.consider(time.Now()) {
			t.log.Info("trigger: %d-byte datagram from %s — the USB entry was selected on the decoder", n, from)
			t.fire()
		}
	}
}

// sourceGuard remembers the decoder's last selection that was not ours.
type sourceGuard struct {
	etc   string
	entry osdEntry
	log   *Logger

	mu       sync.Mutex
	lastName string // birddog-source1-name content
	lastMode string // SourceSelection in the decode settings
	seen     bool
}

func newSourceGuard(etc string, entry osdEntry, log *Logger) *sourceGuard {
	return &sourceGuard{etc: etc, entry: entry, log: log}
}

func (g *sourceGuard) namePath() string     { return filepath.Join(g.etc, sourceNameFile) }
func (g *sourceGuard) settingsPath() string { return filepath.Join(g.etc, decSettingsFile) }

func (g *sourceGuard) currentName() string {
	b, err := os.ReadFile(g.namePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (g *sourceGuard) readSettings() (map[string]any, error) {
	m := map[string]any{}
	b, err := os.ReadFile(g.settingsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (g *sourceGuard) currentMode() string {
	m, err := g.readSettings()
	if err != nil {
		return ""
	}
	s, _ := m["SourceSelection"].(string)
	return s
}

// Observe records the current selection if it is not ours. Called on a
// timer while the decoder runs.
func (g *sourceGuard) Observe() {
	name := g.currentName()
	if g.entry.isOurSelection(name) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.seen || name != g.lastName {
		g.lastName = name
		g.lastMode = g.currentMode()
		g.seen = true
	}
}

// Watch polls until stop is closed.
func (g *sourceGuard) Watch(stop <-chan struct{}, interval time.Duration) {
	g.Observe()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			g.Observe()
		}
	}
}

// Previous reports the remembered selection.
func (g *sourceGuard) Previous() (name, mode string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastName, g.lastMode, g.seen
}

// Restore puts the previous selection back if the decoder currently points
// at us. Returns what it did, for the log.
func (g *sourceGuard) Restore() (string, error) {
	cur := g.currentName()
	if !g.entry.isOurSelection(cur) {
		return "decoder was not pointing at the USB entry; nothing to restore", nil
	}
	name, mode, ok := g.Previous()
	if !ok || name == "" || g.entry.isOurSelection(name) {
		// Never saw anything else. Leave the name alone but make sure the
		// decoder does not come back in SRT mode dialling us: NDI with an
		// unknown source shows the decoder's own splash.
		name = ""
		mode = "NDI"
	}
	if name != "" {
		if err := writeFileAtomic(g.namePath(), []byte(name), 0o644); err != nil {
			return "", fmt.Errorf("restore %s: %w", sourceNameFile, err)
		}
	}
	if mode != "" {
		m, err := g.readSettings()
		if err != nil {
			return "", fmt.Errorf("read %s: %w", decSettingsFile, err)
		}
		if m["SourceSelection"] != mode {
			m["SourceSelection"] = mode
			b, err := json.Marshal(m)
			if err != nil {
				return "", err
			}
			if err := writeFileAtomic(g.settingsPath(), b, 0o644); err != nil {
				return "", fmt.Errorf("restore %s: %w", decSettingsFile, err)
			}
		}
	}
	if name == "" {
		return "decoder source set back to NDI (no earlier selection was seen)", nil
	}
	return fmt.Sprintf("decoder source restored to %q (%s)", name, mode), nil
}
