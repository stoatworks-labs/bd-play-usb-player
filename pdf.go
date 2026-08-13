package main

// PDF support.
//
// The PLAY's rootfs has no PDF renderer of any kind — no poppler, no mupdf, no
// ghostscript, and python3 without PIL. So we ship one: a statically linked
// `mutool` cross-built for aarch64 (see tools/build-mutool.sh). If it is not
// installed, PDFs are listed in the library but flagged as unplayable rather
// than silently ignored.
//
// Pages are rendered once per (document, mode, mtime) and cached, because
// rendering a 40-page deck at 1080p takes long enough on four A53s that doing
// it inside the playback loop would show a black screen between every page.

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PDFRenderer turns PDFs into page images.
//
// Two interchangeable backends, in preference order:
//
//   - bdpdf (PDFium, BSD-3). Preferred, and the only one that can ship from the
//     public web patcher — permissive licence, ~8 MB, and its prebuilt
//     linux-arm64 build needs only GLIBC_2.17.
//   - mutool (MuPDF, AGPL v3). Kept because it works and some units already
//     have it, but it can only be installed by a local fwbuild.
type PDFRenderer struct {
	mu       sync.Mutex
	bdpdf    string // path to bdpdf, or "" if unavailable
	mutool   string // path to mutool, or "" if unavailable
	cacheDir string
	log      *Logger
}

func NewPDFRenderer(cacheDir string, log *Logger) *PDFRenderer {
	r := &PDFRenderer{cacheDir: cacheDir, log: log}
	r.bdpdf = findHelper("bdpdf")
	r.mutool = findMutool()
	switch {
	case r.bdpdf != "":
		r.log.Info("PDF rendering via %s (PDFium)", r.bdpdf)
	case r.mutool != "":
		r.log.Info("PDF rendering via %s (MuPDF)", r.mutool)
	default:
		log.Warn("no PDF renderer found; PDF playback disabled")
	}
	return r
}

// findHelper looks for a helper binary shipped alongside us.
func findHelper(name string) string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, "/userdata/bd-play", "/usr/local/bin")
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func findMutool() string {
	if p, err := exec.LookPath("mutool"); err == nil {
		return p
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, "/userdata/bd-play", "/usr/local/bin")
	for _, d := range dirs {
		p := filepath.Join(d, "mutool")
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func (r *PDFRenderer) Available() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bdpdf != "" || r.mutool != ""
}

// Backend names which renderer is in use, for the status API.
func (r *PDFRenderer) Backend() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.bdpdf != "":
		return "pdfium"
	case r.mutool != "":
		return "mupdf"
	}
	return ""
}

// maxPDFPages caps how much of a document we will render. A 500-page manual on
// a signage loop is a mistake, and rendering it would fill /userdata.
const maxPDFPages = 200

// renderTimeout bounds a single document's render. Four A53s render a normal
// slide in well under a second; a document that blows past this is pathological
// and should not hold up the playlist.
const renderTimeout = 5 * time.Minute

// Render returns the page images for a document, rendering them if needed.
func (r *PDFRenderer) Render(path string, mode Mode) ([]string, error) {
	r.mu.Lock()
	bdpdf, mutool, cacheDir := r.bdpdf, r.mutool, r.cacheDir
	r.mu.Unlock()
	if bdpdf == "" && mutool == "" {
		return nil, fmt.Errorf("no PDF renderer installed")
	}

	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	// Key on content identity and target size, so editing the file on the
	// stick or moving to a different panel re-renders rather than showing a
	// stale deck.
	key := cacheKey(path, st.ModTime(), st.Size(), mode)
	dir := filepath.Join(cacheDir, key)

	if pages, err := listPages(dir); err == nil && len(pages) > 0 {
		return pages, nil
	}

	// Render into a temporary directory and rename into place, so an
	// interrupted render (power cut mid-playlist) cannot leave a half-rendered
	// deck that looks complete on the next boot.
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	r.log.Info("rendering %s at %dx%d", filepath.Base(path), mode.Width, mode.Height)
	start := time.Now()

	// Both backends fit the page inside the panel preserving aspect; the
	// letterboxing is done later by videoscale add-borders, so we do not bake
	// grey bars into the cache and can reuse it if the panel mode changes to
	// another of the same resolution.
	var cmd *exec.Cmd
	if bdpdf != "" {
		cmd = exec.Command(bdpdf,
			"-o", tmp,
			"-w", strconv.Itoa(mode.Width),
			"-h", strconv.Itoa(mode.Height),
			"-n", strconv.Itoa(maxPDFPages),
			path,
		)
	} else {
		// mutool draw -o out/page-%04d.png -w W -h H file.pdf 1-N
		cmd = exec.Command(mutool,
			"draw",
			"-o", filepath.Join(tmp, "page-%04d.png"),
			"-w", strconv.Itoa(mode.Width),
			"-h", strconv.Itoa(mode.Height),
			"-F", "png",
			path,
			"1-"+strconv.Itoa(maxPDFPages),
		)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	renderer := filepath.Base(cmd.Path)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", renderer, err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			// A password-protected or damaged PDF fails here; report the
			// renderer's own message, which names the actual cause.
			return nil, fmt.Errorf("%s: %s", renderer, lastLines(stderr.String(), 2))
		}
	case <-time.After(renderTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-waitCh
		return nil, fmt.Errorf("render timed out after %s", renderTimeout)
	}

	pages, err := listPages(tmp)
	if err != nil || len(pages) == 0 {
		return nil, fmt.Errorf("no pages rendered")
	}

	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		// Fall back to serving straight out of the temp dir this once. It gets
		// cleaned up by the defer, so the next play re-renders, but the
		// current playlist still works.
		r.log.Warn("cache install failed, serving uncached: %v", err)
		return pages, nil
	}

	final, err := listPages(dir)
	if err != nil {
		return nil, err
	}
	r.log.Info("rendered %d page(s) of %s in %s", len(final), filepath.Base(path), time.Since(start).Round(time.Millisecond))
	return final, nil
}

// PageCount renders nothing; it just asks mutool how long the document is, for
// the library listing.
func (r *PDFRenderer) PageCount(path string) int {
	r.mu.Lock()
	bdpdf, mutool := r.bdpdf, r.mutool
	r.mu.Unlock()

	// Both print a "Pages: N" line, so one parser covers them.
	var cmd *exec.Cmd
	switch {
	case bdpdf != "":
		cmd = exec.Command(bdpdf, "-info", path)
	case mutool != "":
		cmd = exec.Command(mutool, "info", path)
	default:
		return 0
	}
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	// "Pages: 12"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Pages:") {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			return n
		}
	}
	return 0
}

func listPages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var pages []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		pages = append(pages, filepath.Join(dir, e.Name()))
	}
	// mutool zero-pads to 4 digits so lexical order is page order, but sort
	// explicitly rather than relying on ReadDir's ordering guarantee.
	sort.Strings(pages)
	return pages, nil
}

func cacheKey(path string, mod time.Time, size int64, mode Mode) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s|%d|%d|%dx%d", path, mod.UnixNano(), size, mode.Width, mode.Height)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// PruneCache drops cached renders older than maxAge, so a device that has
// played many different decks does not slowly fill /userdata.
func (r *PDFRenderer) PruneCache(maxAge time.Duration) {
	entries, err := os.ReadDir(r.cacheDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(r.cacheDir, e.Name()))
	}
}
