package main

// The REST API and the control UI it backs.
//
// CORS is wide open, matching BirdDog's own :8080 API which already sends
// `Access-Control-Allow-Origin: *` on this device. It has to be: the control
// panel is injected into birdUI's page, which is served from :80, so every
// call to us is cross-origin. This is a device on a production LAN with an
// unauthenticated vendor API already listening, so we are not the weak point —
// but it is a deliberate choice, not an oversight, and it is why the daemon
// binds to a configurable address that can be pinned to localhost.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server wires the HTTP surface to the player.
type Server struct {
	storage *StorageManager
	player  *Player
	pdf     *PDFRenderer
	log     *Logger
	// remote is the TV-remote session, nil when -remote=false.
	remote *Remote

	mu      sync.Mutex
	library *Library
	libErr  string
	// lastSelection is remembered so the UI can restore what was playing and
	// so autoplay can resume it when a stick is re-inserted.
	lastSelection Selection
}

// Selection is what the user asked to play.
type Selection struct {
	Path     string   `json:"path"`      // file or folder, relative to volume root
	Paths    []string `json:"paths"`     // explicit multi-select; wins over Path
	Recurse  bool     `json:"recurse"`   // folders: include subfolders
	Order    Order    `json:"order"`     //
	Loop     bool     `json:"loop"`      //
	DwellSec int      `json:"dwell_sec"` //
	Mute     bool     `json:"mute"`      //
}

func NewServer(sm *StorageManager, p *Player, pdf *PDFRenderer, log *Logger) *Server {
	return &Server{storage: sm, player: p, pdf: pdf, log: log}
}

// Rescan rebuilds the library from the primary volume.
func (s *Server) Rescan() {
	vol, ok := s.storage.Primary()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok {
		s.library, s.libErr = nil, ""
		return
	}
	lib, err := ScanVolume(vol.MountPoint, s.pdf.Available())
	if err != nil {
		s.library, s.libErr = nil, err.Error()
		s.log.Warn("scan %s: %v", vol.MountPoint, err)
		return
	}
	s.library, s.libErr = lib, ""
	s.log.Info("library: %d playable item(s) in %d folder(s) on %s",
		len(lib.Playable()), len(lib.Folders), vol.MountPoint)
}

func (s *Server) Library() (*Library, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.library, s.libErr
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/api/status", s.wrap(s.handleStatus))
	mux.HandleFunc("/api/library", s.wrap(s.handleLibrary))
	mux.HandleFunc("/api/play", s.wrap(s.handlePlay))
	mux.HandleFunc("/api/stop", s.wrap(s.handleStop))
	mux.HandleFunc("/api/next", s.wrap(s.handleNext))
	mux.HandleFunc("/api/prev", s.wrap(s.handlePrev))
	mux.HandleFunc("/api/options", s.wrap(s.handleOptions))
	mux.HandleFunc("/api/rescan", s.wrap(s.handleRescan))
	mux.HandleFunc("/api/eject", s.wrap(s.handleEject))
	mux.HandleFunc("/api/log", s.wrap(s.handleLog))
	mux.HandleFunc("/api/remote", s.wrap(s.handleRemote))
	mux.HandleFunc("/api/remote/enter", s.wrap(s.handleRemoteEnter))
	mux.HandleFunc("/api/remote/exit", s.wrap(s.handleRemoteExit))
	mux.HandleFunc("/api/remote/key", s.wrap(s.handleRemoteKey))
	mux.HandleFunc("/bd-play.js", s.handleInjectJS)
	return mux
}

// wrap adds CORS and preflight handling.
func (s *Server) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, a...)})
}

// StatusResponse is the whole visible state in one call, so the UI can poll a
// single endpoint.
type StatusResponse struct {
	Player     Status    `json:"player"`
	Volumes    []Volume  `json:"volumes"`
	Mounted    bool      `json:"mounted"`
	MountPoint string    `json:"mount_point"`
	Label      string    `json:"label"`
	FSType     string    `json:"fs_type"`
	ItemCount  int       `json:"item_count"`
	LibError   string    `json:"lib_error,omitempty"`
	PDFReady   bool      `json:"pdf_ready"`
	PDFBackend string    `json:"pdf_backend,omitempty"`
	ExfatReady bool      `json:"exfat_ready"`
	Selection  Selection `json:"selection"`
	Version    string    `json:"version"`
	// Remote is the TV-remote session, absent when the feature is off.
	Remote *RemoteStatus `json:"remote,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	lib, libErr := s.Library()
	resp := StatusResponse{
		Player:     s.player.Status(),
		Volumes:    s.storage.Volumes(),
		LibError:   libErr,
		PDFReady:   s.pdf.Available(),
		PDFBackend: s.pdf.Backend(),
		Version:    version,
	}
	s.mu.Lock()
	resp.Selection = s.lastSelection
	s.mu.Unlock()
	if s.remote != nil {
		st := s.remote.Status()
		resp.Remote = &st
	}

	s.storage.mu.Lock()
	resp.ExfatReady = s.storage.exfatHelper != ""
	s.storage.mu.Unlock()

	if vol, ok := s.storage.Primary(); ok {
		resp.Mounted = true
		resp.MountPoint = vol.MountPoint
		resp.Label = vol.Label
		resp.FSType = vol.FSType
	}
	if lib != nil {
		resp.ItemCount = len(lib.Playable())
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	lib, libErr := s.Library()
	if lib == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []Item{}, "folders": []Folder{}, "error": libErr,
		})
		return
	}
	writeJSON(w, http.StatusOK, lib)
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	var sel Selection
	if r.Method == http.MethodPost && r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&sel); err != nil && err.Error() != "EOF" {
			writeErr(w, http.StatusBadRequest, "bad request body: %v", err)
			return
		}
	}
	// Query parameters are accepted too, so the whole thing is drivable from
	// curl or a Companion button without composing JSON.
	q := r.URL.Query()
	if v := q.Get("path"); v != "" {
		sel.Path = v
	}
	if v := q.Get("order"); v != "" {
		sel.Order = Order(v)
	}
	if v := q.Get("loop"); v != "" {
		sel.Loop = v == "1" || v == "true"
	}
	if v := q.Get("dwell"); v != "" {
		sel.DwellSec, _ = strconv.Atoi(v)
	}

	if sel.Order != OrderSequential && sel.Order != OrderRandom {
		sel.Order = OrderSequential
	}

	lib, _ := s.Library()
	if lib == nil {
		writeErr(w, http.StatusConflict, "no USB volume mounted")
		return
	}

	var items []Item
	if len(sel.Paths) > 0 {
		items = lib.SelectMany(sel.Paths)
	} else {
		items = lib.Select(sel.Path, sel.Recurse)
	}
	if len(items) == 0 {
		writeErr(w, http.StatusNotFound, "no playable files in that selection")
		return
	}

	dwell := time.Duration(sel.DwellSec) * time.Second
	if dwell <= 0 {
		dwell = DefaultImageDwell
	}

	opts := Options{
		Order:      sel.Order,
		Loop:       sel.Loop,
		ImageDwell: dwell,
		Mute:       sel.Mute,
	}
	// While the TV-remote session owns the display, playback goes through
	// it, so the on-screen browser comes back when the playlist ends
	// instead of the display bouncing to PPApp and back.
	if s.remote != nil && s.remote.Active() {
		if err := s.remote.PlayExternal(items, opts); err != nil {
			writeErr(w, http.StatusInternalServerError, "%v", err)
			return
		}
	} else if err := s.player.Start(items, opts); err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}

	s.mu.Lock()
	s.lastSelection = sel
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(items)})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	// Stop means "give the display back": a running remote session ends
	// too, or the browser would simply reappear.
	if s.remote != nil {
		s.remote.Exit()
	}
	s.player.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// The TV-remote session: status, enter, leave, and key injection — the last
// so the whole browser can be driven from curl or a Companion button, and
// tested without a television.

func (s *Server) handleRemote(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "active": false})
		return
	}
	writeJSON(w, http.StatusOK, s.remote.Status())
}

func (s *Server) handleRemoteEnter(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil {
		writeErr(w, http.StatusConflict, "the TV-remote mode is disabled (-remote=false)")
		return
	}
	if err := s.remote.Enter("requested over the API from " + r.RemoteAddr); err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, s.remote.Status())
}

func (s *Server) handleRemoteExit(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil {
		writeErr(w, http.StatusConflict, "the TV-remote mode is disabled (-remote=false)")
		return
	}
	s.remote.Exit()
	writeJSON(w, http.StatusOK, s.remote.Status())
}

func (s *Server) handleRemoteKey(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil {
		writeErr(w, http.StatusConflict, "the TV-remote mode is disabled (-remote=false)")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" && r.Body != nil {
		var body struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		name = body.Key
	}
	k, ok := ParseKey(name)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown key %q (up, down, left, right, select, back, pageup, pagedown, play, pause, stop, next, prev, info, red, green, yellow, blue, eject)", name)
		return
	}
	if err := s.remote.Key(k); err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": k.String()})
}

func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	s.player.Next()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePrev(w http.ResponseWriter, r *http.Request) {
	s.player.Prev()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleOptions adjusts a running session in place.
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order *string `json:"order"`
		Loop  *bool   `json:"loop"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body.Order != nil {
		o := Order(*body.Order)
		if o == OrderSequential || o == OrderRandom {
			s.player.SetOrder(o)
		}
	}
	if body.Loop != nil {
		s.player.SetLoop(*body.Loop)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	s.storage.Scan()
	s.Rescan()
	lib, _ := s.Library()
	n := 0
	if lib != nil {
		n = len(lib.Playable())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": n})
}

func (s *Server) handleEject(w http.ResponseWriter, r *http.Request) {
	// Stop first: unmounting the filesystem out from under a running pipeline
	// gives GStreamer read errors rather than a clean stop.
	s.player.Stop()
	if err := s.storage.Eject(); err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	s.Rescan()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.log.Lines())
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(controlPageHTML))
}

// handleInjectJS serves the script that birdUI's patched videoset.html loads.
// Served from here rather than copied into /srv/birddog-web-ui/static so that
// upgrading bdplay upgrades the UI too, and so the firmware tree carries
// exactly one modified file.
func (s *Server) handleInjectJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(injectJS))
}

// hostname is used in the UI header so a fleet of PLAYs is distinguishable.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "birddog-play"
	}
	return strings.TrimSpace(h)
}
