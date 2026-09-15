package main

// The on-screen file browser's model: one folder of the library at a time,
// a cursor, and a window of rows that fit on the panel. Pure data so the
// navigation is unit tested; drawing it is osd_render.go's job.

import (
	"fmt"
	"path/filepath"
	"strings"
)

type entryKind int

const (
	entryPlayAll entryKind = iota // play everything in this folder
	entryFolder
	entryFile
	entryUp // ".." back to the parent
)

// Entry is one row of the browser.
type Entry struct {
	Kind   entryKind
	Label  string
	Detail string // right-hand column: size, count, "unplayable"
	Rel    string // folder or file path relative to the volume root
	Item   *Item  // files only
	Count  int    // folders: playable items inside (recursive)
	Grayed bool   // listed but not playable (with the reason in Detail)
}

// Menu is the browser state for one library.
type Menu struct {
	lib     *Library
	dir     string // current folder, "" at the root
	entries []Entry
	cursor  int
	top     int // first visible row
	rows    int // visible rows
	Shuffle bool
	Loop    bool
}

// NewMenu opens the root of a library. rows is how many entries fit on
// screen; a value below 3 is nonsensical and bumped.
func NewMenu(lib *Library, rows int) *Menu {
	if rows < 3 {
		rows = 3
	}
	m := &Menu{lib: lib, rows: rows, Loop: true}
	m.rebuild()
	return m
}

// Dir is the current folder relative to the volume root ("" at the root).
func (m *Menu) Dir() string { return m.dir }

// Entries is the full list for the current folder.
func (m *Menu) Entries() []Entry { return m.entries }

// Cursor is the selected index into Entries.
func (m *Menu) Cursor() int { return m.cursor }

// Visible is the window of entries currently on screen and the index of the
// first one.
func (m *Menu) Visible() (entries []Entry, first int) {
	end := m.top + m.rows
	if end > len(m.entries) {
		end = len(m.entries)
	}
	if m.top > end {
		return nil, 0
	}
	return m.entries[m.top:end], m.top
}

// Selected is the entry under the cursor.
func (m *Menu) Selected() (Entry, bool) {
	if m.cursor < 0 || m.cursor >= len(m.entries) {
		return Entry{}, false
	}
	return m.entries[m.cursor], true
}

// Breadcrumb renders the current path for the header.
func (m *Menu) Breadcrumb() string {
	if m.dir == "" {
		return "/"
	}
	return "/" + m.dir
}

// rebuild lists the current folder: a play-all row, then subfolders, then
// files, each in the library's natural order.
func (m *Menu) rebuild() {
	m.entries = m.entries[:0]
	if m.lib == nil {
		m.cursor, m.top = 0, 0
		return
	}
	if m.dir != "" {
		m.entries = append(m.entries, Entry{Kind: entryUp, Label: "..", Detail: "back", Rel: parentDir(m.dir)})
	}

	total := 0
	for _, f := range m.lib.Folders {
		if f.Rel == m.dir {
			total = f.Total
			break
		}
	}
	if total > 0 {
		label := "Play everything here"
		if m.dir == "" {
			label = "Play everything on the stick"
		}
		m.entries = append(m.entries, Entry{Kind: entryPlayAll, Label: label, Detail: plural(total, "item"), Rel: m.dir, Count: total})
	}

	for _, f := range m.lib.Folders {
		if f.Rel == "" || parentDir(f.Rel) != m.dir || f.Total == 0 {
			continue
		}
		m.entries = append(m.entries, Entry{Kind: entryFolder, Label: f.Name + "/", Detail: plural(f.Total, "item"), Rel: f.Rel, Count: f.Total})
	}

	for i := range m.lib.Items {
		it := &m.lib.Items[i]
		if it.Dir != m.dir {
			continue
		}
		e := Entry{Kind: entryFile, Label: it.Name, Rel: it.Rel, Item: it, Detail: humanSize(it.Size)}
		if it.Skipped != "" {
			e.Grayed = true
			e.Detail = "not playable"
		}
		m.entries = append(m.entries, e)
	}

	if m.cursor >= len(m.entries) {
		m.cursor = len(m.entries) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.clampWindow()
}

// Reload swaps in a rescanned library, keeping the folder if it still exists.
func (m *Menu) Reload(lib *Library) {
	m.lib = lib
	if lib == nil {
		m.dir, m.cursor, m.top = "", 0, 0
		m.rebuild()
		return
	}
	if m.dir != "" && !m.folderExists(m.dir) {
		m.dir, m.cursor, m.top = "", 0, 0
	}
	m.rebuild()
}

func (m *Menu) folderExists(rel string) bool {
	for _, f := range m.lib.Folders {
		if f.Rel == rel {
			return true
		}
	}
	return false
}

// Up, Down, PageUp and PageDown move the cursor; they stop at the ends
// rather than wrapping, which is what every TV menu does.
func (m *Menu) Up()       { m.move(-1) }
func (m *Menu) Down()     { m.move(1) }
func (m *Menu) PageUp()   { m.move(-m.rows) }
func (m *Menu) PageDown() { m.move(m.rows) }

func (m *Menu) move(delta int) {
	if len(m.entries) == 0 {
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.entries) {
		m.cursor = len(m.entries) - 1
	}
	m.clampWindow()
}

func (m *Menu) clampWindow() {
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+m.rows {
		m.top = m.cursor - m.rows + 1
	}
	if m.top < 0 {
		m.top = 0
	}
}

// Enter descends into the folder under the cursor. Returns false if the
// cursor is not on a folder (or the ".." row, which goes up instead).
func (m *Menu) Enter() bool {
	e, ok := m.Selected()
	if !ok {
		return false
	}
	switch e.Kind {
	case entryFolder:
		m.dir = e.Rel
		m.cursor, m.top = 0, 0
		m.rebuild()
		return true
	case entryUp:
		return m.Back()
	}
	return false
}

// Back goes to the parent folder, leaving the cursor on the folder we came
// from. Returns false at the root.
func (m *Menu) Back() bool {
	if m.dir == "" {
		return false
	}
	from := m.dir
	m.dir = parentDir(m.dir)
	m.cursor, m.top = 0, 0
	m.rebuild()
	for i, e := range m.entries {
		if e.Kind == entryFolder && e.Rel == from {
			m.cursor = i
			break
		}
	}
	m.clampWindow()
	return true
}

// Selection describes what the user asked to play from the browser.
type MenuSelection struct {
	Items   []Item
	Single  bool // one file chosen explicitly (a still then holds until a key)
	Label   string
	Recurse bool
}

// SelectionFor works out the play set for the entry under the cursor: a
// single file, or everything in a folder. ok is false for rows that do not
// play (folders to descend into, "..", unplayable files).
func (m *Menu) SelectionFor(e Entry) (MenuSelection, bool) {
	if m.lib == nil {
		return MenuSelection{}, false
	}
	switch e.Kind {
	case entryFile:
		if e.Grayed || e.Item == nil {
			return MenuSelection{}, false
		}
		return MenuSelection{Items: []Item{*e.Item}, Single: true, Label: e.Label}, true
	case entryPlayAll:
		items := m.lib.Select(e.Rel, true)
		if len(items) == 0 {
			return MenuSelection{}, false
		}
		label := "everything on the stick"
		if e.Rel != "" {
			label = "everything in /" + e.Rel
		}
		return MenuSelection{Items: items, Label: label, Recurse: true}, true
	}
	return MenuSelection{}, false
}

// FolderSelection plays the whole current folder, for the Play key.
func (m *Menu) FolderSelection() (MenuSelection, bool) {
	return m.SelectionFor(Entry{Kind: entryPlayAll, Rel: m.dir})
}

func parentDir(rel string) string {
	p := filepath.Dir(rel)
	if p == "." || p == "/" {
		return ""
	}
	return p
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func humanSize(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.0f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	}
	return fmt.Sprintf("%d B", n)
}

// truncateLabel shortens a name to fit a column, keeping the extension
// visible because "holiday_final_v2_(1).mp4" tells you more at the end.
func truncateLabel(s string, max int) string {
	r := []rune(s)
	if len(r) <= max || max < 8 {
		if len(r) > max && max > 0 {
			return string(r[:max])
		}
		return s
	}
	ext := filepath.Ext(s)
	if len([]rune(ext)) > 6 {
		ext = ""
	}
	keepEnd := len([]rune(ext))
	head := max - keepEnd - 1
	if head < 4 {
		return string(r[:max-1]) + "…"
	}
	return strings.TrimSpace(string(r[:head])) + "…" + ext
}
