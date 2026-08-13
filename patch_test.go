package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stockVideoset is the shape of the real file: the Source Selection <select>
// exactly as BirdDog ship it in PLAY firmware 1.0.30/1.0.34, with the Go
// template actions intact.
const stockVideoset = `{{define "videoset"}}
<div class="row m-0 p-1">
  <div class="col-xl-5 m-0 p-0 my-auto">Source Selection</div>
  <div class="col-xl-7 m-0 p-0">
    <span class="custom-dropdown">
      <select id="decode_SourceSelection" name="decode_SourceSelection" onchange='this.form.submit();'>
        <option value="NDI" {{if eq .Source_Selection "NDI"}} selected  {{end}} >NDI</option>
        <option value="CloudConnect" {{if eq .Source_Selection "CloudConnect"}} selected  {{end}} >Cloud Connect</option>
        <option value="SRT" {{if eq .Source_Selection "SRT"}} selected  {{end}} >SRT</option>
      </select>
    </span>
  </div>
</div>
{{end}}
`

func writeStock(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "videoset.html")
	if err := os.WriteFile(path, []byte(stockVideoset), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPatchAddsUSBOption(t *testing.T) {
	path := writeStock(t)
	if err := PatchVideoset(path, ":8091"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)

	if !strings.Contains(got, `<option value="USB"`) {
		t.Error("USB option missing after patch")
	}
	if !strings.Contains(got, patchMarker) {
		t.Error("patch marker missing")
	}
	if !strings.Contains(got, "bd-play.js") {
		t.Error("script loader missing")
	}

	// The stock options must survive untouched — this file drives the real
	// decode path and a mangled template takes the whole web UI down.
	for _, stock := range []string{
		`<option value="NDI" {{if eq .Source_Selection "NDI"}} selected  {{end}} >NDI</option>`,
		`<option value="CloudConnect" {{if eq .Source_Selection "CloudConnect"}} selected  {{end}} >Cloud Connect</option>`,
		`<option value="SRT" {{if eq .Source_Selection "SRT"}} selected  {{end}} >SRT</option>`,
	} {
		if !strings.Contains(got, stock) {
			t.Errorf("stock option was altered or lost:\n%s", stock)
		}
	}

	// Go template actions must be intact, or html/template fails to parse and
	// birddog-web-ui serves an error for every page.
	if strings.Count(got, "{{define") != 1 || strings.Count(got, "{{end}}") != strings.Count(stockVideoset, "{{end}}") {
		t.Error("template actions were disturbed by the patch")
	}
}

func TestPatchIsIdempotent(t *testing.T) {
	path := writeStock(t)
	if err := PatchVideoset(path, ":8091"); err != nil {
		t.Fatal(err)
	}
	after1 := read(t, path)

	err := PatchVideoset(path, ":8091")
	if !errors.Is(err, errAlreadyPatched) {
		t.Fatalf("second patch returned %v, want errAlreadyPatched", err)
	}
	if got := read(t, path); got != after1 {
		t.Error("second patch modified the file")
	}
	if n := strings.Count(after1, `<option value="USB"`); n != 1 {
		t.Errorf("USB option appears %d times, want 1", n)
	}
}

func TestPatchUnpatchRoundTrip(t *testing.T) {
	path := writeStock(t)
	original := read(t, path)

	if err := PatchVideoset(path, ":8091"); err != nil {
		t.Fatal(err)
	}
	if err := UnpatchVideoset(path); err != nil {
		t.Fatal(err)
	}

	if got := read(t, path); got != original {
		t.Errorf("unpatch did not restore the file byte-for-byte\n--- got ---\n%s\n--- want ---\n%s", got, original)
	}
	// The backup must be cleaned up, so a later firmware update is not silently
	// reverted by a stale copy.
	if _, err := os.Stat(path + stockSuffix); !os.IsNotExist(err) {
		t.Error("stock backup was left behind after unpatch")
	}
}

// Losing the backup must not leave the page broken; strip the injection
// textually instead.
func TestUnpatchWithoutBackup(t *testing.T) {
	path := writeStock(t)
	if err := PatchVideoset(path, ":8091"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + stockSuffix); err != nil {
		t.Fatal(err)
	}
	if err := UnpatchVideoset(path); err != nil {
		t.Fatal(err)
	}

	got := read(t, path)
	if strings.Contains(got, patchMarker) {
		t.Error("injection survived a backup-less unpatch")
	}
	if strings.Contains(got, `<option value="USB"`) {
		t.Error("USB option survived a backup-less unpatch")
	}
	if !strings.Contains(got, `<option value="SRT"`) {
		t.Error("backup-less unpatch damaged the stock options")
	}
	if !strings.Contains(got, "{{define") {
		t.Error("backup-less unpatch damaged the template")
	}
}

// Re-patching for a different port must move the page to the new port rather
// than leaving it pointed at a daemon that is no longer there.
func TestPatchRetargetsChangedPort(t *testing.T) {
	path := writeStock(t)
	if err := PatchVideoset(path, ":8091"); err != nil {
		t.Fatal(err)
	}
	if err := PatchVideoset(path, ":9100"); err != nil {
		t.Fatalf("re-patch with a new port: %v", err)
	}
	got := read(t, path)
	if !strings.Contains(got, "api=:9100") {
		t.Error("re-patch did not record the new port")
	}
	if strings.Contains(got, "api=:8091") {
		t.Error("old port survived the re-patch")
	}
	if n := strings.Count(got, `<option value="USB"`); n != 1 {
		t.Errorf("USB option appears %d times after re-patch, want 1", n)
	}
}

// Whitespace drift between firmware revisions must not stop the patch.
func TestPatchToleratesWhitespaceDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "videoset.html")
	drifted := strings.Replace(stockVideoset,
		`<option value="SRT" {{if eq .Source_Selection "SRT"}} selected  {{end}} >SRT</option>`,
		`<option value="SRT"   {{if eq .Source_Selection "SRT"}}selected{{end}}>SRT</option>`, 1)
	if err := os.WriteFile(path, []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := PatchVideoset(path, ":8091"); err != nil {
		t.Fatalf("patch failed on a whitespace-drifted template: %v", err)
	}
	got := read(t, path)
	if !strings.Contains(got, `<option value="USB"`) {
		t.Error("USB option missing")
	}
	if !strings.Contains(got, `>SRT</option>`) {
		t.Error("the drifted SRT option was damaged")
	}
}

// An unrecognisable file must be refused outright, not half-patched.
func TestPatchRefusesUnknownLayout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "videoset.html")
	content := "<html><body>nothing we recognise</body></html>"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := PatchVideoset(path, ":8091"); err == nil {
		t.Fatal("patch succeeded on an unrecognised layout")
	}
	if got := read(t, path); got != content {
		t.Error("a refused patch still modified the file")
	}
}

func TestPatchMissingFile(t *testing.T) {
	if err := PatchVideoset(filepath.Join(t.TempDir(), "nope.html"), ":8091"); err == nil {
		t.Error("patching a missing file should fail")
	}
}
