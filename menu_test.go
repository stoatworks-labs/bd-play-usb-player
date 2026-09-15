package main

import (
	"strings"
	"testing"
)

// menuLibrary scans the shared test stick: three clips and a pptx at the
// root, photos/ with two images and photos/nested/ with one.
func menuLibrary(t *testing.T) *Library {
	t.Helper()
	lib, err := ScanVolume(buildTree(t), true)
	if err != nil {
		t.Fatal(err)
	}
	return lib
}

func labels(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Label
	}
	return out
}

func TestMenuRootListing(t *testing.T) {
	m := NewMenu(menuLibrary(t), 10)
	got := labels(m.Entries())
	want := []string{"Play everything on the stick", "photos/", "clip1.mp4", "clip2.mp4", "clip10.mp4", "deck.pptx"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("root = %v, want %v", got, want)
	}
	if e := m.Entries()[5]; !e.Grayed || e.Detail != "not playable" {
		t.Errorf("the pptx should be listed grayed with a reason, got %+v", e)
	}
	if e := m.Entries()[1]; e.Kind != entryFolder || e.Count != 3 || e.Detail != "3 items" {
		t.Errorf("photos/ should count its nested content, got %+v", e)
	}
	if m.Breadcrumb() != "/" {
		t.Errorf("breadcrumb = %q", m.Breadcrumb())
	}
}

func TestMenuNavigation(t *testing.T) {
	m := NewMenu(menuLibrary(t), 10)
	m.Up() // at the top already: stays
	if m.Cursor() != 0 {
		t.Fatal("Up at the top moved")
	}
	m.Down()
	if e, _ := m.Selected(); e.Label != "photos/" {
		t.Fatalf("Down selected %q", e.Label)
	}
	if !m.Enter() {
		t.Fatal("Enter on a folder should descend")
	}
	if m.Dir() != "photos" || m.Breadcrumb() != "/photos" {
		t.Fatalf("dir = %q", m.Dir())
	}
	got := labels(m.Entries())
	want := []string{"..", "Play everything here", "nested/", "a.jpg", "b.png"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("photos = %v, want %v", got, want)
	}
	// Enter on ".." goes up, and the cursor lands on the folder we left.
	if !m.Enter() {
		t.Fatal("Enter on .. should go up")
	}
	if e, _ := m.Selected(); m.Dir() != "" || e.Label != "photos/" {
		t.Fatalf("after going up: dir %q, selected %q", m.Dir(), e.Label)
	}
	if m.Back() {
		t.Error("Back at the root should report false")
	}
	// Enter on a file does nothing to the folder.
	m.Down()
	m.Down()
	if m.Enter() {
		t.Error("Enter on a file should not descend")
	}
	m.PageDown()
	if m.Cursor() != len(m.Entries())-1 {
		t.Errorf("PageDown should stop at the last entry, cursor %d", m.Cursor())
	}
	m.PageUp()
	if m.Cursor() != 0 {
		t.Errorf("PageUp should stop at the first entry, cursor %d", m.Cursor())
	}
}

func TestMenuSelection(t *testing.T) {
	m := NewMenu(menuLibrary(t), 10)

	all, ok := m.SelectionFor(m.Entries()[0])
	if !ok || len(all.Items) != 6 || all.Single || !all.Recurse {
		t.Fatalf("play-all = %+v, %v", all, ok)
	}
	if _, ok := m.SelectionFor(m.Entries()[1]); ok {
		t.Error("a folder row is entered, not played")
	}
	one, ok := m.SelectionFor(m.Entries()[2])
	if !ok || len(one.Items) != 1 || !one.Single || one.Items[0].Name != "clip1.mp4" {
		t.Fatalf("single = %+v, %v", one, ok)
	}
	if _, ok := m.SelectionFor(m.Entries()[5]); ok {
		t.Error("the grayed pptx must not be selectable")
	}

	m.Down()
	m.Enter() // photos
	folder, ok := m.FolderSelection()
	if !ok || len(folder.Items) != 3 || folder.Label != "everything in /photos" {
		t.Fatalf("folder selection = %+v, %v", folder, ok)
	}
}

func TestMenuWindowFollowsCursor(t *testing.T) {
	items := make([]Item, 0, 30)
	for i := 0; i < 30; i++ {
		items = append(items, Item{Name: "f" + string(rune('a'+i%26)) + ".mp4", Rel: "x", Kind: KindVideo})
	}
	lib := &Library{Items: items, Folders: []Folder{{Rel: "", Name: "/", Count: 30, Total: 30}}}
	m := NewMenu(lib, 5)
	if v, first := m.Visible(); len(v) != 5 || first != 0 {
		t.Fatalf("initial window %d@%d", len(v), first)
	}
	for i := 0; i < 7; i++ {
		m.Down()
	}
	v, first := m.Visible()
	if first != 3 || len(v) != 5 || m.Cursor() != 7 {
		t.Errorf("after 7 downs: window %d@%d cursor %d, want 5@3 cursor 7", len(v), first, m.Cursor())
	}
	m.PageDown()
	if m.Cursor() != 12 {
		t.Errorf("PageDown moved to %d, want 12", m.Cursor())
	}
	for i := 0; i < 40; i++ {
		m.Down()
	}
	v, first = m.Visible()
	if m.Cursor() != 30 || first != 26 || len(v) != 5 {
		t.Errorf("at the end: cursor %d window %d@%d", m.Cursor(), len(v), first)
	}
}

func TestMenuReload(t *testing.T) {
	lib := menuLibrary(t)
	m := NewMenu(lib, 10)
	m.Down()
	m.Enter() // photos
	m.Down()
	m.Down()
	m.Enter() // photos/nested
	if m.Dir() != "photos/nested" {
		t.Fatalf("dir = %q", m.Dir())
	}
	// A rescan without that folder falls back to the root.
	m.Reload(&Library{Items: lib.Items[:1], Folders: []Folder{{Rel: "", Name: "/", Count: 1, Total: 1}}})
	if m.Dir() != "" || len(m.Entries()) != 2 {
		t.Errorf("after reload: dir %q, %d entries", m.Dir(), len(m.Entries()))
	}
	m.Reload(nil)
	if len(m.Entries()) != 0 {
		t.Error("no library, no entries")
	}
	m.Down()
	if _, ok := m.Selected(); ok {
		t.Error("nothing to select on an empty menu")
	}
}

func TestTruncateLabel(t *testing.T) {
	cases := map[string]string{
		"short.mp4": "short.mp4",
		"a_very_long_file_name_that_goes_on_and_on.mp4": "a_very_long_file_na….mp4",
		"no extension here at all but long enough":      "no extension here at al…",
	}
	for in, want := range cases {
		if got := truncateLabel(in, 24); got != want {
			t.Errorf("truncateLabel(%q, 24) = %q, want %q", in, got, want)
		}
	}
	if humanSize(1536) != "2 KB" || humanSize(5*1024*1024) != "5 MB" || humanSize(3*1024*1024*1024/2) != "1.5 GB" {
		t.Error("humanSize")
	}
}
