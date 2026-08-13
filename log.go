package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Logger writes to stderr (picked up by journald via the systemd unit) and
// keeps a small ring buffer the web UI can show, so diagnosing a stick that
// will not play does not require SSH.
type Logger struct {
	mu   sync.Mutex
	w    io.Writer
	ring []LogLine
	max  int
}

type LogLine struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

func NewLogger() *Logger {
	return &Logger{w: os.Stderr, max: 200}
}

func (l *Logger) emit(level, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	now := time.Now()

	l.mu.Lock()
	l.ring = append(l.ring, LogLine{Time: now, Level: level, Text: text})
	if len(l.ring) > l.max {
		l.ring = l.ring[len(l.ring)-l.max:]
	}
	w := l.w
	l.mu.Unlock()

	fmt.Fprintf(w, "%s [%s] %s\n", now.Format("15:04:05"), level, text)
}

func (l *Logger) Info(f string, a ...any)  { l.emit("info", f, a...) }
func (l *Logger) Warn(f string, a ...any)  { l.emit("warn", f, a...) }
func (l *Logger) Error(f string, a ...any) { l.emit("error", f, a...) }

// Lines returns a copy of the ring buffer, oldest first.
func (l *Logger) Lines() []LogLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LogLine, len(l.ring))
	copy(out, l.ring)
	return out
}
