package main

// Putting "USB" into birdUI's Source Selection dropdown.
//
// The dropdown lives in /srv/birddog-web-ui/videoset.html, a Go html/template
// that birddog-web-ui parses FROM DISK ON EVERY REQUEST — verified on hardware:
// editing the file changes the served page immediately, with no service restart
// and no recompile. That is the whole reason this integration is possible.
//
// What we cannot do is teach the compiled Go handler what "USB" means. The
// stock <select> submits the form, the Go binary validates the value against
// its own NDI/CloudConnect/SRT allowlist, and the Express API on :8080 does not
// accept SourceSelection at all (it validates every other field and simply has
// no branch for it). So the option alone would be inert at best.
//
// Instead we add the option AND a script that intercepts it: choosing USB stops
// the form submitting and hands over to bdplay. BirdDog's own SourceSelection
// value is never set to USB, so the stock decode path sees a device that is
// still configured for NDI and behaves normally the moment we release it.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errAlreadyPatched = errors.New("already patched")

const (
	// patchMarker identifies our injection so the patch is idempotent and
	// removable. It also lets a human grepping the firmware find out what
	// touched the file.
	patchMarker = "bdplay-injected"

	// anchor is the stock SRT option. It is the last entry in the select, so
	// inserting after it puts USB at the bottom of the list.
	anchor = `<option value="SRT" {{if eq .Source_Selection "SRT"}} selected  {{end}} >SRT</option>`
)

// stockSuffix names the pristine backup we keep beside the patched file.
const stockSuffix = ".bdplay-stock"

// PatchVideoset adds the USB option and our script to birdUI.
func PatchVideoset(path, apiAddr string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content := string(raw)

	if strings.Contains(content, patchMarker) {
		// Re-patch anyway if the API port changed, so upgrading the daemon
		// with a different -addr does not leave the page pointing at nothing.
		if strings.Contains(content, apiPortComment(apiAddr)) {
			return errAlreadyPatched
		}
		if err := UnpatchVideoset(path); err != nil {
			return fmt.Errorf("re-patch: %w", err)
		}
		raw, err = os.ReadFile(path)
		if err != nil {
			return err
		}
		content = string(raw)
	}

	if !strings.Contains(content, anchor) {
		// Try a looser match before giving up: BirdDog reflow whitespace
		// between firmware versions, and we would rather patch than refuse.
		loose := looseAnchor(content)
		if loose == "" {
			return fmt.Errorf("could not find the Source Selection dropdown in %s (unrecognised firmware layout)", path)
		}
		content = strings.Replace(content, loose, loose+usbOption(), 1)
	} else {
		content = strings.Replace(content, anchor, anchor+usbOption(), 1)
	}

	// Keep a pristine copy so unpatching is exact rather than a reverse-sed.
	backup := path + stockSuffix
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		if err := os.WriteFile(backup, raw, 0o644); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}

	content += "\n" + scriptTag(apiAddr) + "\n"

	return writeFileAtomic(path, []byte(content), 0o644)
}

// usbOption is the dropdown entry. It is never "selected" from the template's
// point of view because BirdDog's stored SourceSelection never becomes USB;
// our script marks it selected client-side when bdplay reports it is playing.
func usbOption() string {
	return `<option value="USB" data-` + patchMarker + `="1">USB</option>`
}

// apiPortComment records which bdplay address the page was patched for.
func apiPortComment(addr string) string {
	return fmt.Sprintf("<!-- %s api=%s -->", patchMarker, addr)
}

func scriptTag(addr string) string {
	port := portOf(addr)
	// The script is fetched from bdplay itself, on the same host the page came
	// from, so a fleet of units needs no per-unit configuration.
	return apiPortComment(addr) +
		`<script>(function(){var s=document.createElement('script');` +
		`s.src=location.protocol+'//'+location.hostname+':` + port + `/bd-play.js';` +
		`s.async=true;s.setAttribute('data-` + patchMarker + `','1');` +
		// Failing silently is deliberate: if bdplay is stopped, birdUI must
		// still work exactly as it did before.
		`s.onerror=function(){};document.body.appendChild(s);})();</script>`
}

// looseAnchor finds the SRT option with whitespace normalised, returning the
// exact substring present in the file so the caller can splice around it.
func looseAnchor(content string) string {
	idx := strings.Index(content, `<option value="SRT"`)
	if idx < 0 {
		return ""
	}
	end := strings.Index(content[idx:], "</option>")
	if end < 0 {
		return ""
	}
	return content[idx : idx+end+len("</option>")]
}

// UnpatchVideoset restores the pristine file.
func UnpatchVideoset(path string) error {
	backup := path + stockSuffix
	raw, err := os.ReadFile(backup)
	if err != nil {
		if os.IsNotExist(err) {
			// No backup: strip our additions textually. Less exact, but it
			// must not leave the page broken.
			return stripPatch(path)
		}
		return err
	}
	if err := writeFileAtomic(path, raw, 0o644); err != nil {
		return err
	}
	return os.Remove(backup)
}

func stripPatch(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(raw)
	if !strings.Contains(content, patchMarker) {
		return nil
	}
	var kept []string
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, patchMarker) {
			// A line that is only our injection goes entirely; a line that
			// merely contains the option (because it was spliced inline) has
			// just the option removed.
			if opt := usbOption(); strings.Contains(line, opt) {
				line = strings.ReplaceAll(line, opt, "")
				if strings.TrimSpace(line) == "" {
					continue
				}
				kept = append(kept, line)
			}
			continue
		}
		kept = append(kept, line)
	}
	return writeFileAtomic(path, []byte(strings.Join(kept, "\n")), 0o644)
}

// writeFileAtomic writes via a temp file and rename, so a crash mid-write
// cannot leave birdUI serving a truncated template — which would take the
// whole web UI down, not just our page.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bdplay-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
