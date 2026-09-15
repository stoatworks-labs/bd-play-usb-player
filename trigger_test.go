package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOSDEntryRegisterMergesWithOtherEntries(t *testing.T) {
	etc := t.TempDir()
	list := filepath.Join(etc, srtListFile)
	// A list the stock page wrote, with a key we do not know about.
	stock := `{"streams":[{"name":"Encoder","uri":"10.0.0.5:9000?mode=caller&latency=120"}],"note":"keep me"}`
	if err := os.WriteFile(list, []byte(stock), 0o644); err != nil {
		t.Fatal(err)
	}
	e := osdEntry{etc: etc, name: "USB-Player", port: 9107}
	changed, err := e.Register()
	if err != nil || !changed {
		t.Fatalf("Register: changed=%v err=%v", changed, err)
	}
	changed, err = e.Register()
	if err != nil || changed {
		t.Fatalf("second Register should be a no-op: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(list)
	var got struct {
		Streams []srtEntry `json:"streams"`
		Note    string     `json:"note"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Note != "keep me" {
		t.Error("unknown keys must survive")
	}
	if len(got.Streams) != 2 || got.Streams[0].Name != "Encoder" || got.Streams[1].Name != "USB-Player" {
		t.Fatalf("streams = %+v", got.Streams)
	}
	if got.Streams[1].URI != "127.0.0.1:9107?mode=caller&latency=120" {
		t.Errorf("uri = %q", got.Streams[1].URI)
	}
	// The stock page parses the URI by position: exactly host:port, mode, latency.
	parts := strings.Split(got.Streams[1].URI, "&")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "127.0.0.1:9107?mode=caller") || !strings.HasPrefix(parts[1], "latency=") {
		t.Errorf("uri shape the stock page cannot parse: %q", got.Streams[1].URI)
	}

	// A port change refreshes the entry in place.
	e2 := osdEntry{etc: etc, name: "USB-Player", port: 9200}
	if changed, err := e2.Register(); err != nil || !changed {
		t.Fatalf("port change: changed=%v err=%v", changed, err)
	}
	if changed, err := e2.Unregister(); err != nil || !changed {
		t.Fatalf("Unregister: changed=%v err=%v", changed, err)
	}
	b, _ = os.ReadFile(list)
	if strings.Contains(string(b), "USB-Player") || !strings.Contains(string(b), "Encoder") || !strings.Contains(string(b), "keep me") {
		t.Errorf("after Unregister: %s", b)
	}
	if changed, _ := e2.Unregister(); changed {
		t.Error("Unregister twice should be a no-op")
	}
}

func TestOSDEntryStartsFromNothing(t *testing.T) {
	etc := t.TempDir()
	e := osdEntry{etc: etc, name: "USB-Player", port: 9107}
	if _, err := e.Register(); err != nil {
		t.Fatal(err)
	}
	l, err := readSRTList(e.listPath())
	if err != nil || len(l.Streams) != 1 {
		t.Fatalf("list = %+v, %v", l, err)
	}
	// Garbage in the file is replaced rather than fatal.
	os.WriteFile(e.listPath(), []byte("{not json"), 0o644)
	if _, err := e.Register(); err != nil {
		t.Fatal(err)
	}
	l, _ = readSRTList(e.listPath())
	if len(l.Streams) != 1 {
		t.Errorf("after garbage: %+v", l)
	}
	if !e.isOurSelection("SRT:USB-Player(127.0.0.1:9107?mode=caller&latency=120)") {
		t.Error("selection value not recognised")
	}
	if e.isOurSelection("SRT:USB-Player2(x)") || e.isOurSelection("MACHINE (USB-Player)") {
		t.Error("false positive on the selection value")
	}
}

func TestSourceGuardRestoresThePreviousSelection(t *testing.T) {
	etc := t.TempDir()
	e := osdEntry{etc: etc, name: "USB-Player", port: 9107}
	g := newSourceGuard(etc, e, NewLogger())
	name := filepath.Join(etc, sourceNameFile)
	settings := filepath.Join(etc, decSettingsFile)
	os.WriteFile(name, []byte("STUDIO (Cam 1)\n"), 0o644)
	os.WriteFile(settings, []byte(`{"ColorSpace":"YUV","SourceSelection":"NDI","ChNum":1}`), 0o644)

	g.Observe()
	// The decoder's menu picks our entry (and, we assume, persists it).
	os.WriteFile(name, []byte(e.selectionValue()), 0o644)
	os.WriteFile(settings, []byte(`{"ColorSpace":"YUV","SourceSelection":"SRT","ChNum":1}`), 0o644)
	g.Observe() // must not overwrite the remembered value with ours

	what, err := g.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(what, "STUDIO (Cam 1)") {
		t.Errorf("restore said %q", what)
	}
	b, _ := os.ReadFile(name)
	if string(b) != "STUDIO (Cam 1)" {
		t.Errorf("name file = %q", b)
	}
	var m map[string]any
	b, _ = os.ReadFile(settings)
	json.Unmarshal(b, &m)
	if m["SourceSelection"] != "NDI" || m["ColorSpace"] != "YUV" || m["ChNum"] != float64(1) {
		t.Errorf("settings = %v", m)
	}

	// Not pointing at us: nothing is touched.
	os.WriteFile(name, []byte("SRT:Encoder(10.0.0.5:9000?mode=caller&latency=120)"), 0o644)
	what, err = g.Restore()
	if err != nil || !strings.Contains(what, "nothing to restore") {
		t.Errorf("restore = %q, %v", what, err)
	}
}

func TestSourceGuardWithNoHistoryFallsBackToNDI(t *testing.T) {
	etc := t.TempDir()
	e := osdEntry{etc: etc, name: "USB-Player", port: 9107}
	g := newSourceGuard(etc, e, NewLogger())
	name := filepath.Join(etc, sourceNameFile)
	settings := filepath.Join(etc, decSettingsFile)
	os.WriteFile(name, []byte(e.selectionValue()), 0o644)
	os.WriteFile(settings, []byte(`{"SourceSelection":"SRT"}`), 0o644)
	g.Observe()
	what, err := g.Restore()
	if err != nil || !strings.Contains(what, "NDI") {
		t.Fatalf("restore = %q, %v", what, err)
	}
	b, _ := os.ReadFile(settings)
	if !strings.Contains(string(b), `"SourceSelection":"NDI"`) {
		t.Errorf("settings = %s", b)
	}
	b, _ = os.ReadFile(name)
	if string(b) != e.selectionValue() {
		t.Errorf("the name file should be left alone with no history, got %q", b)
	}
}

func TestTriggerDebounce(t *testing.T) {
	tr := newTriggerListener(0, NewLogger(), func() {})
	now := time.Now()
	if !tr.consider(now) {
		t.Fatal("first datagram should fire")
	}
	if tr.consider(now.Add(time.Second)) {
		t.Error("a retransmit a second later should not fire again")
	}
	if !tr.consider(now.Add(6 * time.Second)) {
		t.Error("a fresh selection after the burst should fire")
	}
	tr.Suspend()
	if tr.consider(now.Add(20 * time.Second)) {
		t.Error("suspended: nothing fires")
	}
	tr.Resume()
	if tr.consider(time.Now().Add(5 * time.Second)) {
		t.Error("the quiet period after Resume should swallow the decoder's retries")
	}
	if !tr.consider(time.Now().Add(20 * time.Second)) {
		t.Error("after the quiet period it fires again")
	}
}

func TestTriggerListenerFiresOnADatagram(t *testing.T) {
	// Find a free loopback port.
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	fired := make(chan struct{}, 4)
	tr := newTriggerListener(port, NewLogger(), func() { fired <- struct{}{} })
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- tr.Run(stop) }()

	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// An SRT caller's induction handshake is a 64-byte UDP packet; the
	// contents do not matter to us.
	for i := 0; i < 3; i++ {
		if _, err := conn.Write(make([]byte, 64)); err != nil {
			// The listener may not be up yet on the first try.
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("trigger never fired")
	}
	select {
	case <-fired:
		t.Error("the retransmits fired the trigger again")
	case <-time.After(200 * time.Millisecond):
	}
	close(stop)
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not stop")
	}
}
