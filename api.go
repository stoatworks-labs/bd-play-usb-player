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

	if err := s.player.Start(items, Options{
		Order:      sel.Order,
		Loop:       sel.Loop,
		ImageDwell: dwell,
		Mute:       sel.Mute,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}

	s.mu.Lock()
	s.lastSelection = sel
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(items)})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.player.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
