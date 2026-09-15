package main

// Putting rendered frames on HDMI.
//
// The browser is not video, but on this device the only way to the panel is
// kmssink, and kmssink wants a stream. So one long-lived gst-launch process
// takes raw frames on its stdin and shows each as it arrives; the session
// writes a frame only when something changed, and the plane keeps scanning
// out the last one in between. Frames are RGBA straight from the renderer;
// videoconvert is in the chain so the exact plane format never matters.
//
// Two pipelines, because one plugin is not certain to be on the box:
//
//   raw  fdsrc ! rawvideoparse ! videoconvert ! kmssink   (preferred)
//   png  multifilesrc re-reading a PNG we replace atomically (fallback)
//
// rawvideoparse is what turns a byte stream into frame-sized buffers — a
// pipe delivers short reads, so fdsrc alone would hand kmssink fragments.
// It ships in gst-plugins-bad alongside kmssink, but the plugin set on a
// vendor rootfs is whatever they chose to build, so it is probed once and
// the PNG route used if it is missing. The PNG route re-decodes a panel-sized
// image several times a second: a few percent of one core, acceptable for a
// menu that is on screen while nothing else runs.
//
// Only one kmssink can drive the display at a time — the first process to
// open the DRM node is its master — so the session stops this pipeline
// before it starts a playback one, and restarts it afterwards.

import (
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const (
	screenRaw = "raw"
	screenPNG = "png"
)

// Screen is the frame sink.
type Screen struct {
	kind string
	dir  string // scratch for the PNG route
	log  *Logger

	mu    sync.Mutex
	mode  Mode
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// pngPath is the file multifilesrc re-reads.
	pngPath string
}

// NewScreen picks the pipeline: "auto" probes for rawvideoparse.
func NewScreen(kind, dir string, log *Logger) *Screen {
	s := &Screen{kind: kind, dir: dir, log: log}
	if kind == "" || kind == "auto" {
		if gstHasElement("rawvideoparse") {
			s.kind = screenRaw
		} else {
			log.Warn("gst: rawvideoparse not available; the on-screen browser will use the PNG pipeline")
			s.kind = screenPNG
		}
	}
	return s
}

// Kind reports which pipeline is in use.
func (s *Screen) Kind() string { return s.kind }

// gstHasElement asks gst-inspect whether an element exists. Only called at
// startup; gst-inspect takes a moment on this CPU.
func gstHasElement(name string) bool {
	return exec.Command("gst-inspect-1.0", "--exists", name).Run() == nil
}

// pipelineArgs builds the gst-launch arguments for a panel size.
func (s *Screen) pipelineArgs(mode Mode) []string {
	switch s.kind {
	case screenPNG:
		return []string{
			"multifilesrc", "location=" + s.pngPath, "loop=true",
			"caps=image/png,framerate=5/1",
			"!", "pngdec",
			"!", "videoconvert",
			"!", "kmssink", "sync=false",
		}
	default:
		return []string{
			"fdsrc", "fd=0", "blocksize=262144",
			"!", "rawvideoparse", "use-sink-caps=false", "format=rgba",
			fmt.Sprintf("width=%d", mode.Width), fmt.Sprintf("height=%d", mode.Height),
			"framerate=10/1",
			"!", "videoconvert",
			"!", "kmssink", "sync=false",
		}
	}
}

// Start launches the pipeline for the given panel size. The first frame must
// follow promptly: kmssink sets its mode when the first buffer arrives, and
// until then the panel shows whatever was there.
func (s *Screen) Start(mode Mode, first []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return nil
	}
	s.mode = mode
	if s.kind == screenPNG {
		if err := os.MkdirAll(s.dir, 0o755); err != nil {
			return err
		}
		s.pngPath = filepath.Join(s.dir, "osd.png")
		if err := s.writePNGLocked(first); err != nil {
			return err
		}
	}

	cmd := exec.Command(gstBin, append([]string{"-q"}, s.pipelineArgs(mode)...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	var stdin io.WriteCloser
	if s.kind == screenRaw {
		w, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdin = w
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", gstBin, err)
	}
	s.cmd, s.stdin = cmd, stdin
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		exited := s.cmd == cmd
		if exited {
			s.cmd, s.stdin = nil, nil
		}
		s.mu.Unlock()
		if exited && err != nil {
			s.log.Warn("osd pipeline exited: %v: %s", err, lastLines(stderr.String(), 3))
		}
	}()

	if s.kind == screenRaw {
		if err := s.writeRawLocked(first); err != nil {
			return err
		}
	}
	return nil
}

// Show puts a frame on the panel. A no-op if the pipeline is not running.
func (s *Screen) Show(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return fmt.Errorf("osd pipeline not running")
	}
	if s.kind == screenPNG {
		return s.writePNGLocked(frame)
	}
	return s.writeRawLocked(frame)
}

func (s *Screen) writeRawLocked(frame []byte) error {
	if s.stdin == nil {
		return fmt.Errorf("osd pipeline has no stdin")
	}
	want := s.mode.Width * s.mode.Height * 4
	if len(frame) != want {
		return fmt.Errorf("frame is %d bytes, panel needs %d", len(frame), want)
	}
	_, err := s.stdin.Write(frame)
	return err
}

func (s *Screen) writePNGLocked(frame []byte) error {
	img := &image.RGBA{Pix: frame, Stride: s.mode.Width * 4, Rect: image.Rect(0, 0, s.mode.Width, s.mode.Height)}
	tmp := s.pngPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(f, img); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.pngPath)
}

// Stop tears the pipeline down and waits for it, so the DRM plane is free
// for whatever comes next.
func (s *Screen) Stop() {
	s.mu.Lock()
	cmd, stdin := s.cmd, s.stdin
	s.cmd, s.stdin = nil, nil
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	terminate(cmd)
	if s.kind == screenPNG && s.pngPath != "" {
		_ = os.Remove(s.pngPath)
	}
}

// Running reports whether the pipeline is up.
func (s *Screen) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}
