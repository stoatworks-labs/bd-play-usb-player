package main

import (
	"strings"
	"testing"
)

// Filenames on a USB stick routinely contain spaces, ampersands and hashes.
// A bare path in a file:// URI silently truncates at '#' and mis-parses on
// '?', so the clip plays the wrong file or none at all.
func TestFileURIEscaping(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/media/usb0/clip.mp4", "file:///media/usb0/clip.mp4"},
		{"/media/usb0/my clip.mp4", "file:///media/usb0/my%20clip.mp4"},
		{"/media/usb0/a#b.mp4", "file:///media/usb0/a%23b.mp4"},
		{"/media/usb0/a?b.mp4", "file:///media/usb0/a%3Fb.mp4"},
		{"/media/usb0/a&b.mp4", "file:///media/usb0/a%26b.mp4"},
		{"/media/usb0/100%.mp4", "file:///media/usb0/100%25.mp4"},
		{"/media/usb0/a'b.mp4", "file:///media/usb0/a%27b.mp4"},
		// Unreserved characters must pass through unescaped, or GStreamer
		// resolves a path that does not exist.
		{"/media/usb0/a-b_c.d~e.mp4", "file:///media/usb0/a-b_c.d~e.mp4"},
	}
	for _, c := range cases {
		if got := fileURI(c.in); got != c.want {
			t.Errorf("fileURI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Non-ASCII names (accented, CJK) must survive as UTF-8 percent-encoding.
func TestFileURIUnicode(t *testing.T) {
	got := fileURI("/media/usb0/café.mp4")
	if strings.ContainsAny(got, "é") {
		t.Errorf("fileURI left raw non-ASCII bytes in %q", got)
	}
	if !strings.HasPrefix(got, "file:///media/usb0/caf") || !strings.HasSuffix(got, ".mp4") {
		t.Errorf("fileURI mangled the path: %q", got)
	}
}

func TestImagePipelineUsesDetectedMode(t *testing.T) {
	p := &Player{mode: Mode{Width: 3840, Height: 2160}}
	args := strings.Join(p.imagePipeline("/media/usb0/a.png"), " ")

	// Stills must be rendered at the panel's real resolution: scaling a 4K
	// panel's still at 1080p and letting the VOP upscale visibly softens text.
	if !strings.Contains(args, "width=3840,height=2160") {
		t.Errorf("pipeline did not use the detected mode: %s", args)
	}
	// add-borders keeps slide aspect ratio instead of stretching.
	if !strings.Contains(args, "add-borders=true") {
		t.Errorf("pipeline would stretch the image: %s", args)
	}
	if !strings.Contains(args, "kmssink") {
		t.Errorf("pipeline does not output to KMS: %s", args)
	}
}

func TestVideoPipelineMute(t *testing.T) {
	p := &Player{mode: Mode{Width: 1920, Height: 1080}}

	loud := strings.Join(p.videoPipeline("/media/usb0/a.mp4"), " ")
	if !strings.Contains(loud, "alsasink device="+hdmiAudioDevice) {
		t.Errorf("audio should go to the HDMI card: %s", loud)
	}

	p.opts.Mute = true
	quiet := strings.Join(p.videoPipeline("/media/usb0/a.mp4"), " ")
	if strings.Contains(quiet, "alsasink") {
		t.Errorf("muted pipeline still opens the audio device: %s", quiet)
	}
	if !strings.Contains(quiet, "fakesink") {
		t.Errorf("muted pipeline should sink audio to fakesink: %s", quiet)
	}
}

func TestLastLines(t *testing.T) {
	in := "one\ntwo\nthree\nfour\nfive"
	if got := lastLines(in, 2); got != "four; five" {
		t.Errorf("lastLines = %q", got)
	}
	if got := lastLines("only", 3); got != "only" {
		t.Errorf("lastLines short input = %q", got)
	}
}

func TestPortOf(t *testing.T) {
	cases := map[string]string{
		":8091":          "8091",
		"0.0.0.0:8091":   "8091",
		"127.0.0.1:9100": "9100",
		"[::1]:8091":     "8091",
	}
	for in, want := range cases {
		if got := portOf(in); got != want {
			t.Errorf("portOf(%q) = %q, want %q", in, got, want)
		}
	}
}
