package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNaturalLess(t *testing.T) {
	// The case this exists for: lexical sort puts slide10 before slide2, which
	// silently reorders a deck.
	cases := []struct {
		a, b string
		want bool
	}{
		{"slide2.jpg", "slide10.jpg", true},
		{"slide10.jpg", "slide2.jpg", false},
		{"slide02.jpg", "slide2.jpg", false}, // equal numerically, longer pad sorts after
		{"a.jpg", "b.jpg", true},
		{"B.jpg", "a.jpg", false}, // case-insensitive: b sorts after a
		{"A.jpg", "b.jpg", true},  // ...and case does not flip the result
		{"clip.mp4", "clip2.mp4", true},
		{"1.jpg", "10.jpg", true},
		{"100.jpg", "99.jpg", false},
	}
	for _, c := range cases {
		if got := naturalLess(c.a, c.b); got != c.want {
			t.Errorf("naturalLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"a.mp4":  KindVideo,
		"a.MP4":  KindVideo,
		"a.mkv":  KindVideo,
		"a.jpg":  KindImage,
		"a.PNG":  KindImage,
		"a.pdf":  KindPDF,
		"a.txt":  "",
		"a.pptx": "",
		"noext":  "",
	}
	for name, want := range cases {
		got, ok := classify(name)
		if want == "" {
			if ok {
				t.Errorf("classify(%q) = %v, want unclassified", name, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("classify(%q) = %v,%v want %v", name, got, ok, want)
		}
	}
}

// buildTree writes a small stick layout into a temp dir.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"clip1.mp4",
		"clip10.mp4",
		"clip2.mp4",
		"readme.txt",
		"deck.pptx",
		"photos/a.jpg",
		"photos/b.png",
		"photos/nested/c.jpg",
		".hidden.jpg",
		"._resource.jpg",
		"System Volume Information/junk.jpg",
		".Trashes/old.mp4",
	}
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScanVolume(t *testing.T) {
	root := buildTree(t)
	lib, err := ScanVolume(root, false)
	if err != nil {
		t.Fatal(err)
	}

	playable := lib.Playable()
	got := make([]string, len(playable))
	for i, it := range playable {
		got[i] = it.Rel
	}

	// Root files first (natural order), then photos/, then photos/nested/.
	want := []string{
		"clip1.mp4", "clip2.mp4", "clip10.mp4",
		filepath.Join("photos", "a.jpg"),
		filepath.Join("photos", "b.png"),
		filepath.Join("photos", "nested", "c.jpg"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d playable items %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %q, want %q", i, got[i], want[i])
		}
	}

	// Hidden files, AppleDouble forks and the Windows/macOS metadata
	// directories must never reach the playlist.
	for _, it := range lib.Items {
		switch {
		case it.Name == ".hidden.jpg", it.Name == "._resource.jpg":
			t.Errorf("hidden file %q leaked into the library", it.Rel)
		case it.Dir == "System Volume Information", it.Dir == ".Trashes":
			t.Errorf("metadata directory %q leaked into the library", it.Rel)
		}
	}

	// The .pptx should be present but flagged, so the UI can explain itself
	// rather than the file just vanishing.
	var foundDeck bool
	for _, it := range lib.Items {
		if it.Name == "deck.pptx" {
			foundDeck = true
			if it.Skipped == "" {
				t.Error("deck.pptx should carry a Skipped reason")
			}
		}
	}
	if !foundDeck {
		t.Error("deck.pptx missing from the listing entirely")
	}
}

func TestScanVolumePDFDisabled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}

	lib, err := ScanVolume(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Playable()) != 0 {
		t.Error("PDF should not be playable when rendering is unavailable")
	}
	if len(lib.Items) != 1 || lib.Items[0].Skipped == "" {
		t.Errorf("PDF should be listed with a reason, got %+v", lib.Items)
	}

	lib, err = ScanVolume(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Playable()) != 1 {
		t.Error("PDF should be playable when rendering is available")
	}
}

func TestFolderCounts(t *testing.T) {
	root := buildTree(t)
	lib, err := ScanVolume(root, false)
	if err != nil {
		t.Fatal(err)
	}
	byRel := map[string]Folder{}
	for _, f := range lib.Folders {
		byRel[f.Rel] = f
	}

	// The root's Total counts everything; photos counts itself plus nested.
	if got := byRel[""].Total; got != 6 {
		t.Errorf("root total = %d, want 6", got)
	}
	if got := byRel[""].Count; got != 3 {
		t.Errorf("root direct count = %d, want 3", got)
	}
	if got := byRel["photos"].Total; got != 3 {
		t.Errorf("photos total = %d, want 3 (including nested)", got)
	}
	if got := byRel["photos"].Count; got != 2 {
		t.Errorf("photos direct count = %d, want 2", got)
	}
}

func TestSelect(t *testing.T) {
	root := buildTree(t)
	lib, err := ScanVolume(root, false)
	if err != nil {
		t.Fatal(err)
	}

	if got := len(lib.Select("", true)); got != 6 {
		t.Errorf("whole stick = %d items, want 6", got)
	}
	if got := len(lib.Select("photos", true)); got != 3 {
		t.Errorf("photos recursive = %d, want 3", got)
	}
	if got := len(lib.Select("photos", false)); got != 2 {
		t.Errorf("photos non-recursive = %d, want 2", got)
	}

	// Selecting one file yields exactly that file, whatever recurse says.
	one := lib.Select("clip2.mp4", true)
	if len(one) != 1 || one[0].Name != "clip2.mp4" {
		t.Errorf("single file select = %+v, want just clip2.mp4", one)
	}

	if got := len(lib.Select("nope", true)); got != 0 {
		t.Errorf("missing folder = %d items, want 0", got)
	}
}

func TestSelectManyKeepsLibraryOrder(t *testing.T) {
	root := buildTree(t)
	lib, err := ScanVolume(root, false)
	if err != nil {
		t.Fatal(err)
	}
	// Ask in a deliberately scrambled order; the result must come back in
	// natural order, which is what "play these in order" means.
	got := lib.SelectMany([]string{"clip10.mp4", "clip1.mp4", "clip2.mp4"})
	want := []string{"clip1.mp4", "clip2.mp4", "clip10.mp4"}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Errorf("item %d = %q, want %q", i, got[i].Name, want[i])
		}
	}
}

// A selection must not be able to escape the volume via traversal.
func TestSelectRejectsTraversal(t *testing.T) {
	root := buildTree(t)
	lib, err := ScanVolume(root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sel := range []string{"../../etc", "photos/../../etc", "/etc"} {
		if got := lib.Select(sel, true); len(got) != 0 {
			t.Errorf("Select(%q) returned %d items, want 0", sel, len(got))
		}
	}
}
