package main

// Scanning a mounted volume into a browsable, playable library.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind classifies what we can do with a file.
type Kind string

const (
	KindVideo Kind = "video"
	KindImage Kind = "image"
	KindPDF   Kind = "pdf"
)

// Item is one playable file.
type Item struct {
	Path    string `json:"path"`    // absolute path on the device
	Rel     string `json:"rel"`     // path relative to the volume root, for display
	Name    string `json:"name"`    // base name
	Kind    Kind   `json:"kind"`    //
	Size    int64  `json:"size"`    //
	Dir     string `json:"dir"`     // parent, relative to volume root ("" at root)
	Pages   int    `json:"pages"`   // PDF only, 0 until rendered
	Skipped string `json:"skipped"` // non-empty if unplayable, with the reason
}

// Folder is a directory containing playable items.
type Folder struct {
	Rel   string `json:"rel"`   // "" is the volume root
	Name  string `json:"name"`  // display name; "/" at the root
	Count int    `json:"count"` // playable items directly inside
	Total int    `json:"total"` // playable items including subfolders
}

// Library is the result of one scan of one volume.
type Library struct {
	Root    string   `json:"root"`
	Items   []Item   `json:"items"`
	Folders []Folder `json:"folders"`
}

// Extension tables. Deliberately conservative: everything here is something
// the device can actually decode.
//
// Video is whatever mppvideodec or a software fallback handles inside a
// container GStreamer's typefind recognises. The MPP decoder covers H.264,
// HEVC, VP8/VP9 and MPEG-4; anything else falls back to software on four A53s
// and will likely stutter above SD, which is why the exotic codecs are absent
// rather than silently disappointing.
var videoExt = map[string]bool{
	".mp4": true, ".m4v": true, ".mov": true, ".mkv": true,
	".avi": true, ".ts": true, ".m2ts": true, ".mts": true,
	".mpg": true, ".mpeg": true, ".webm": true, ".wmv": true,
}

// Images: jpegdec/pngdec are present, plus libwebp/libtiff on the device.
// BMP and GIF go through GStreamer's own decoders.
var imageExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".bmp": true,
	".gif": true, ".webp": true, ".tif": true, ".tiff": true,
}

var pdfExt = map[string]bool{".pdf": true}

// officeExt are formats we deliberately do not render. They are listed so the
// UI can say "export this to PDF" instead of silently ignoring the file, which
// otherwise looks like the stick failed to read.
var officeExt = map[string]bool{
	".ppt": true, ".pptx": true, ".key": true, ".odp": true,
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
}

func classify(name string) (Kind, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case videoExt[ext]:
		return KindVideo, true
	case imageExt[ext]:
		return KindImage, true
	case pdfExt[ext]:
		return KindPDF, true
	}
	return "", false
}

// maxScanEntries caps a scan so a stick with a pathological number of files
// cannot pin the CPU or exhaust the 2 GB of RAM. Well beyond any real playlist.
const maxScanEntries = 20000

// ScanVolume walks a mount point and builds the library.
//
// Hidden files and the junk directories that macOS and Windows sprinkle on
// removable media are skipped — without this, a stick written on a Mac plays a
// stream of AppleDouble resource forks as "images", which looks like a bug.
func ScanVolume(root string, pdfEnabled bool) (*Library, error) {
	lib := &Library{Root: root}
	perDir := map[string]int{}
	subtree := map[string]int{}
	count := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// An unreadable directory should not abort the whole scan.
			return nil //nolint:nilerr // deliberate: skip and continue
		}
		if count >= maxScanEntries {
			return filepath.SkipAll
		}
		name := info.Name()
		if info.IsDir() {
			if path != root && skipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "._") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			dir = ""
		}

		kind, ok := classify(name)
		if !ok {
			if officeExt[strings.ToLower(filepath.Ext(name))] {
				lib.Items = append(lib.Items, Item{
					Path: path, Rel: rel, Name: name, Dir: dir, Size: info.Size(),
					Skipped: "Office documents are not rendered on the device — export to PDF",
				})
			}
			return nil
		}
		if kind == KindPDF && !pdfEnabled {
			lib.Items = append(lib.Items, Item{
				Path: path, Rel: rel, Name: name, Dir: dir, Size: info.Size(), Kind: KindPDF,
				Skipped: "PDF rendering is not installed on this device",
			})
			return nil
		}

		lib.Items = append(lib.Items, Item{
			Path: path, Rel: rel, Name: name, Kind: kind, Size: info.Size(), Dir: dir,
		})
		count++
		perDir[dir]++
		// Credit every ancestor, so a folder's Total includes nested content.
		for d := dir; ; {
			subtree[d]++
			if d == "" {
				break
			}
			parent := filepath.Dir(d)
			if parent == "." {
				parent = ""
			}
			d = parent
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortItems(lib.Items)

	for rel, total := range subtree {
		name := "/"
		if rel != "" {
			name = filepath.Base(rel)
		}
		lib.Folders = append(lib.Folders, Folder{
			Rel: rel, Name: name, Count: perDir[rel], Total: total,
		})
	}
	sort.Slice(lib.Folders, func(i, j int) bool { return lib.Folders[i].Rel < lib.Folders[j].Rel })
	return lib, nil
}

// skipDir filters the metadata directories removable media accumulate.
func skipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true // .Trashes, .Spotlight-V100, .fseventsd
	}
	switch strings.ToUpper(name) {
	case "SYSTEM VOLUME INFORMATION", "$RECYCLE.BIN", "RECYCLER", "FOUND.000", "LOST+DIR":
		return true
	}
	return false
}

// sortItems orders items the way a person laying out a playlist expects:
// by folder, then by name with embedded numbers compared numerically, so
// "slide2" precedes "slide10".
func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Dir != items[j].Dir {
			return items[i].Dir < items[j].Dir
		}
		return naturalLess(items[i].Name, items[j].Name)
	})
}

// naturalLess compares strings treating digit runs as numbers.
func naturalLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(la) && j < len(lb) {
		ca, cb := la[i], lb[j]
		if isDigit(ca) && isDigit(cb) {
			si, sj := i, j
			for i < len(la) && isDigit(la[i]) {
				i++
			}
			for j < len(lb) && isDigit(lb[j]) {
				j++
			}
			na := strings.TrimLeft(la[si:i], "0")
			nb := strings.TrimLeft(lb[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	return len(la)-i < len(lb)-j
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Playable returns only the items that can actually be played.
func (l *Library) Playable() []Item {
	out := make([]Item, 0, len(l.Items))
	for _, it := range l.Items {
		if it.Skipped == "" {
			out = append(out, it)
		}
	}
	return out
}

// Select builds a play set from a user selection.
//
// sel is either a file's path relative to the volume root, a folder's relative
// path, or "" meaning the whole stick. recurse controls whether a folder pulls
// in its subfolders.
func (l *Library) Select(sel string, recurse bool) []Item {
	sel = strings.Trim(filepath.Clean("/"+sel), "/")
	if sel == "." {
		sel = ""
	}
	var out []Item
	for _, it := range l.Playable() {
		switch {
		case sel == "":
			out = append(out, it)
		case it.Rel == sel:
			// Exact file match: a single-item playlist.
			return []Item{it}
		case recurse && (it.Dir == sel || strings.HasPrefix(it.Dir, sel+"/")):
			out = append(out, it)
		case !recurse && it.Dir == sel:
			out = append(out, it)
		}
	}
	return out
}

// SelectMany builds a play set from an explicit list of relative paths,
// preserving the library's natural order rather than the order they were
// clicked — which is what "play these in order" means for a folder of slides.
func (l *Library) SelectMany(rels []string) []Item {
	want := make(map[string]bool, len(rels))
	for _, r := range rels {
		want[strings.Trim(filepath.Clean("/"+r), "/")] = true
	}
	var out []Item
	for _, it := range l.Playable() {
		if want[it.Rel] {
			out = append(out, it)
		}
	}
	return out
}
