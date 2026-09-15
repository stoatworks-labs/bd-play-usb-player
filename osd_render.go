package main

// Drawing the browser. Pure Go: an *image.RGBA the size of the panel, text
// from the bundled Go fonts, no compositor and nothing from the device. The
// bytes come out in the order GStreamer calls "RGBA", which is what
// screen.go feeds to kmssink.
//
// Everything is sized from the panel height so a 720p TV and a 4K one get
// the same layout. Glyphs are restricted to what the Go fonts carry — no
// arrows or media symbols, which is why the legend draws its own icons.

import (
	"image"
	"image/color"
	"image/draw"
	"path/filepath"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	colBackground = color.RGBA{0x14, 0x17, 0x1c, 0xff}
	colHeader     = color.RGBA{0x1d, 0x22, 0x2a, 0xff}
	colText       = color.RGBA{0xe6, 0xea, 0xef, 0xff}
	colMuted      = color.RGBA{0x8a, 0x93, 0x9e, 0xff}
	colGrayed     = color.RGBA{0x5c, 0x64, 0x6e, 0xff}
	colFolder     = color.RGBA{0xf5, 0xc5, 0x5a, 0xff}
	colCursor     = color.RGBA{0x2e, 0x6f, 0xd6, 0xff}
	colCursorText = color.RGBA{0xff, 0xff, 0xff, 0xff}
	colBadge      = color.RGBA{0x2c, 0x33, 0x3d, 0xff}
	colAccent     = color.RGBA{0xe8, 0x44, 0x2a, 0xff}
	colGood       = color.RGBA{0x3d, 0xb8, 0x6b, 0xff}

	colKeyRed    = color.RGBA{0xe0, 0x3c, 0x31, 0xff}
	colKeyGreen  = color.RGBA{0x3d, 0xb8, 0x6b, 0xff}
	colKeyYellow = color.RGBA{0xf2, 0xc9, 0x4c, 0xff}
	colKeyBlue   = color.RGBA{0x3a, 0x7b, 0xe8, 0xff}
)

// legendItem is one hint in the footer; Color draws a coloured dot before
// the text for the TV remote's colour keys.
type legendItem struct {
	Color color.RGBA
	Text  string
}

// osdView is everything one frame shows. The session fills it, the renderer
// draws it; neither knows about the other's internals.
type osdView struct {
	Title      string
	Subtitle   string // volume label and filesystem, or a status
	Breadcrumb string

	Entries []Entry // the visible window
	First   int     // absolute index of Entries[0]
	Cursor  int     // absolute index of the selected row
	Total   int     // rows in the folder, for the scroll indicator

	Status    string // one line above the legend, e.g. what is playing
	StatusBad bool   // draw Status in the warning colour
	Legend    []legendItem

	// Message, when set, replaces the list with centred text — no stick,
	// starting up, that sort of thing.
	Message []string
}

// Renderer draws osdViews at one panel size.
type Renderer struct {
	w, h    int
	img     *image.RGBA
	regular *opentype.Font
	bold    *opentype.Font
	faces   map[faceKey]font.Face
	// row geometry, derived from h
	rowH    int
	listTop int
	listBot int
}

type faceKey struct {
	bold bool
	size int
}

func NewRenderer(w, h int) (*Renderer, error) {
	reg, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, err
	}
	bld, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, err
	}
	r := &Renderer{
		w: w, h: h,
		img:     image.NewRGBA(image.Rect(0, 0, w, h)),
		regular: reg, bold: bld,
		faces: map[faceKey]font.Face{},
	}
	r.rowH = h / 22
	r.listTop = h * 17 / 100
	r.listBot = h * 84 / 100
	return r, nil
}

// Rows is how many list entries fit on the panel.
func (r *Renderer) Rows() int {
	n := (r.listBot - r.listTop) / r.rowH
	if n < 3 {
		return 3
	}
	return n
}

// Frame is the last rendered frame: w*h*4 bytes, RGBA.
func (r *Renderer) Frame() []byte { return r.img.Pix }

func (r *Renderer) face(bold bool, size int) font.Face {
	k := faceKey{bold, size}
	if f, ok := r.faces[k]; ok {
		return f
	}
	src := r.regular
	if bold {
		src = r.bold
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		// Cannot happen for the bundled fonts; fall back to something
		// visible rather than crashing the session over a glyph cache.
		f = basicFace
	}
	r.faces[k] = f
	return f
}

// Draw renders the view and returns the frame bytes (owned by the renderer;
// copy if keeping).
func (r *Renderer) Draw(v osdView) []byte {
	r.fill(r.img.Bounds(), colBackground)
	r.drawHeader(v)
	if len(v.Message) > 0 {
		r.drawMessage(v.Message)
	} else {
		r.drawList(v)
	}
	r.drawFooter(v)
	return r.img.Pix
}

func (r *Renderer) drawHeader(v osdView) {
	h := r.h
	r.fill(image.Rect(0, 0, r.w, h*12/100), colHeader)
	r.fill(image.Rect(0, h*12/100, r.w, h*12/100+h/270+1), colAccent)

	margin := r.w * 4 / 100
	title := r.face(true, h/20)
	r.text(v.Title, margin, h*8/100, title, colText)

	if v.Subtitle != "" {
		sub := r.face(false, h/36)
		tw := r.measure(v.Subtitle, sub)
		r.text(v.Subtitle, r.w-margin-tw, h*8/100, sub, colMuted)
	}
	if v.Breadcrumb != "" {
		bc := r.face(false, h/32)
		r.text(truncateLabel(v.Breadcrumb, 80), margin, r.listTop-h/60, bc, colMuted)
	}
}

func (r *Renderer) drawList(v osdView) {
	margin := r.w * 4 / 100
	body := r.face(false, r.h/30)
	small := r.face(true, r.h/54)
	detailX := r.w - margin
	iconW := r.rowH * 5 / 4
	textX := margin + iconW + r.rowH/3
	maxLabel := (detailX - textX - r.w/8) / (r.h / 60) // rough chars-per-row from the em size

	y := r.listTop
	for i, e := range v.Entries {
		abs := v.First + i
		row := image.Rect(margin-r.rowH/4, y, r.w-margin+r.rowH/4, y+r.rowH)
		selected := abs == v.Cursor
		fg := colText
		if selected {
			r.fill(row, colCursor)
			fg = colCursorText
		} else if e.Grayed {
			fg = colGrayed
		} else if e.Kind == entryFolder || e.Kind == entryUp {
			fg = colFolder
		}

		r.drawIcon(e, margin, y, iconW, selected)
		baseline := y + r.rowH*70/100
		r.text(truncateLabel(e.Label, maxLabel), textX, baseline, body, fg)

		if e.Detail != "" {
			dc := colMuted
			if selected {
				dc = colCursorText
			}
			dw := r.measure(e.Detail, small)
			r.text(e.Detail, detailX-dw, baseline, small, dc)
		}
		y += r.rowH
	}

	// Scroll indicator: a thin track on the right when the folder does not fit.
	if v.Total > len(v.Entries) && len(v.Entries) > 0 {
		track := image.Rect(r.w-margin/2, r.listTop, r.w-margin/2+r.w/400+1, r.listTop+len(v.Entries)*r.rowH)
		r.fill(track, colHeader)
		span := track.Dy()
		thumbH := span * len(v.Entries) / v.Total
		if thumbH < r.rowH/2 {
			thumbH = r.rowH / 2
		}
		thumbY := track.Min.Y + (span-thumbH)*v.First/(v.Total-len(v.Entries)+1)
		r.fill(image.Rect(track.Min.X, thumbY, track.Max.X, thumbY+thumbH), colMuted)
	}
}

// drawIcon draws a small glyph for the row's kind: a play triangle, a folder
// tab, or a badge with the file's extension.
func (r *Renderer) drawIcon(e Entry, x, y, w int, selected bool) {
	cy := y + r.rowH/2
	switch e.Kind {
	case entryPlayAll:
		c := colGood
		if selected {
			c = colCursorText
		}
		r.triangle(x+w/4, cy-r.rowH/4, x+w/4+r.rowH/2, cy, r.rowH/2, c)
	case entryFolder:
		c := colFolder
		if selected {
			c = colCursorText
		}
		fh := r.rowH * 55 / 100
		fw := w * 70 / 100
		fx := x + (w-fw)/2
		fy := cy - fh/2
		r.fill(image.Rect(fx, fy, fx+fw*45/100, fy+fh/5), c)                   // tab
		r.fill(image.Rect(fx, fy+fh/6, fx+fw, fy+fh), c)                       // body
		r.fill(image.Rect(fx+2, fy+fh/6+2, fx+fw-2, fy+fh/6+4), colBackground) // fold line
	case entryUp:
		c := colFolder
		if selected {
			c = colCursorText
		}
		// A left-pointing chevron.
		r.triangle(x+w*60/100, cy-r.rowH/4, x+w*25/100, cy, r.rowH/2, c)
	case entryFile:
		ext := strings.ToUpper(strings.TrimPrefix(filepath.Ext(e.Label), "."))
		if len(ext) > 4 {
			ext = ext[:4]
		}
		bg := colBadge
		fg := colMuted
		if selected {
			bg = colCursorText
			fg = colCursor
		} else if e.Grayed {
			fg = colGrayed
		}
		bh := r.rowH * 55 / 100
		bx := x + (w-w*85/100)/2
		r.fill(image.Rect(bx, cy-bh/2, bx+w*85/100, cy+bh/2), bg)
		f := r.face(true, r.h/60)
		tw := r.measure(ext, f)
		r.text(ext, bx+(w*85/100-tw)/2, cy+bh*30/100, f, fg)
	}
}

func (r *Renderer) drawMessage(lines []string) {
	big := r.face(true, r.h/22)
	body := r.face(false, r.h/32)
	lineH := r.h / 18
	total := lineH * len(lines)
	y := (r.listTop+r.listBot)/2 - total/2 + lineH*2/3
	for i, s := range lines {
		f, c := body, colMuted
		if i == 0 {
			f, c = big, colText
		}
		tw := r.measure(s, f)
		r.text(s, (r.w-tw)/2, y, f, c)
		y += lineH
	}
}

func (r *Renderer) drawFooter(v osdView) {
	h := r.h
	margin := r.w * 4 / 100
	r.fill(image.Rect(0, h*88/100, r.w, h), colHeader)

	if v.Status != "" {
		f := r.face(false, h/34)
		c := colText
		if v.StatusBad {
			c = colAccent
		}
		r.text(truncateLabel(v.Status, 110), margin, r.listBot+h*3/100, f, c)
	}

	f := r.face(false, h/40)
	x := margin
	baseline := h*94/100 + h/120
	dot := h / 90
	for _, it := range v.Legend {
		if it.Color.A != 0 {
			r.disc(x+dot, baseline-dot, dot, it.Color)
			x += dot*2 + dot
		}
		r.text(it.Text, x, baseline, f, colMuted)
		x += r.measure(it.Text, f) + r.w*2/100
		if x > r.w-margin {
			break
		}
	}
}

// Primitives.

func (r *Renderer) fill(rect image.Rectangle, c color.RGBA) {
	draw.Draw(r.img, rect.Intersect(r.img.Bounds()), &image.Uniform{c}, image.Point{}, draw.Src)
}

func (r *Renderer) text(s string, x, baseline int, f font.Face, c color.RGBA) {
	d := &font.Drawer{Dst: r.img, Src: &image.Uniform{c}, Face: f, Dot: fixed.P(x, baseline)}
	d.DrawString(s)
}

func (r *Renderer) measure(s string, f font.Face) int {
	d := &font.Drawer{Face: f}
	return d.MeasureString(s).Ceil()
}

// triangle fills an isosceles triangle with its tip at (tipX, midY) and its
// base at baseX spanning height h — pointing right if tipX > baseX.
func (r *Renderer) triangle(baseX, topY, tipX, midY, h int, c color.RGBA) {
	dir := 1
	if tipX < baseX {
		dir = -1
	}
	width := (tipX - baseX) * dir
	if width <= 0 {
		return
	}
	for dx := 0; dx <= width; dx++ {
		half := h/2 - dx*h/(2*width)
		x := baseX + dx*dir
		r.fill(image.Rect(x, midY-half, x+1, midY+half+1), c)
	}
}

func (r *Renderer) disc(cx, cy, rad int, c color.RGBA) {
	for dy := -rad; dy <= rad; dy++ {
		for dx := -rad; dx <= rad; dx++ {
			if dx*dx+dy*dy <= rad*rad {
				r.img.SetRGBA(cx+dx, cy+dy, c)
			}
		}
	}
}

// basicFace is the bitmap fallback if a vector face cannot be built.
var basicFace font.Face = basicfont.Face7x13
