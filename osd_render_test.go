package main

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"testing"
)

func testView(t *testing.T) osdView {
	t.Helper()
	m := NewMenu(menuLibrary(t), 5)
	m.Down()
	entries, first := m.Visible()
	return osdView{
		Title:      "USB Player",
		Subtitle:   "STICK (vfat)",
		Breadcrumb: m.Breadcrumb(),
		Entries:    entries, First: first, Cursor: m.Cursor(), Total: len(m.Entries()),
		Status: "TV remote ready (CEC address 4)  ·  shuffle off  ·  loop on",
		Legend: []legendItem{{Text: "OK open or play"}, {Color: colKeyRed, Text: "BirdDog"}},
	}
}

func TestRendererFrameShape(t *testing.T) {
	r, err := NewRenderer(320, 180)
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows() < 3 {
		t.Errorf("rows = %d", r.Rows())
	}
	frame := r.Draw(testView(t))
	if len(frame) != 320*180*4 {
		t.Fatalf("frame is %d bytes, want %d", len(frame), 320*180*4)
	}
	// Alpha is opaque everywhere: kmssink gets RGBA and a transparent frame
	// would be a black screen.
	for i := 3; i < len(frame); i += 4 {
		if frame[i] != 0xff {
			t.Fatalf("pixel %d has alpha %d", i/4, frame[i])
		}
	}
}

func TestRendererHighlightsTheCursorRow(t *testing.T) {
	r, err := NewRenderer(640, 360)
	if err != nil {
		t.Fatal(err)
	}
	v := testView(t)
	r.Draw(v)
	img := r.img
	// The cursor is on row 1 (relative to the window's first row).
	rowIdx := v.Cursor - v.First
	y := r.listTop + rowIdx*r.rowH + r.rowH/2
	x := r.w / 2
	if c := img.RGBAAt(x, y); c != colCursor {
		t.Errorf("cursor row pixel = %v, want %v", c, colCursor)
	}
	other := r.listTop + (rowIdx+1)*r.rowH + r.rowH/2
	if c := img.RGBAAt(x, other); c == colCursor {
		t.Error("the row below the cursor is highlighted too")
	}
}

func TestRendererIsDeterministic(t *testing.T) {
	r, err := NewRenderer(320, 180)
	if err != nil {
		t.Fatal(err)
	}
	v := testView(t)
	a := append([]byte(nil), r.Draw(v)...)
	b := r.Draw(v)
	if !bytes.Equal(a, b) {
		t.Error("two renders of the same view differ")
	}
	v.Message = []string{"No USB storage found", "Insert a stick."}
	c := r.Draw(v)
	if bytes.Equal(a, c) {
		t.Error("a message screen should not look like the list")
	}
}

// TestRendererWritesAPreview saves a 1080p frame when OSD_PREVIEW is set, so
// the layout can be eyeballed without a device: OSD_PREVIEW=out.png go test
// -run Preview .
func TestRendererWritesAPreview(t *testing.T) {
	out := os.Getenv("OSD_PREVIEW")
	if out == "" {
		t.Skip("set OSD_PREVIEW=<file.png> to write a preview frame")
	}
	r, err := NewRenderer(1920, 1080)
	if err != nil {
		t.Fatal(err)
	}
	v := testView(t)
	v.Legend = []legendItem{
		{Text: "Up/Down choose"}, {Text: "OK open or play"}, {Text: "Exit back"}, {Text: "Play this folder"},
		{Text: "Stop leave"}, {Color: colKeyRed, Text: "BirdDog"}, {Color: colKeyBlue, Text: "shuffle"}, {Color: colKeyYellow, Text: "loop"},
	}
	r.Draw(v)
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := &image.RGBA{Pix: r.Frame(), Stride: 1920 * 4, Rect: image.Rect(0, 0, 1920, 1080)}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
