package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// LogLine is one entry in the in-app session log.
type LogLine struct {
	Seq     int64  `json:"seq"`
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// logBuffer keeps a bounded ring of recent log lines for the UI.
type logBuffer struct {
	mu    sync.Mutex
	lines []LogLine
	next  int64
	max   int
}

func newLogBuffer(max int) *logBuffer {
	if max < 1 {
		max = 1
	}
	return &logBuffer{max: max}
}

func (b *logBuffer) add(level slog.Level, message string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	line := LogLine{
		Seq:     b.next,
		Time:    time.Now().Format("15:04:05"),
		Level:   level.String(),
		Message: message,
	}
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

func (b *logBuffer) snapshot(since int64) []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) == 0 {
		return nil
	}
	// Lines are ordered, so find the first entry after since.
	start := len(b.lines)
	for i := len(b.lines) - 1; i >= 0; i-- {
		if b.lines[i].Seq <= since {
			break
		}
		start = i
	}
	return append([]LogLine(nil), b.lines[start:]...)
}

// bufferHandler routes slog records into the UI log buffer.
type bufferHandler struct {
	buf   *logBuffer
	level slog.Level
	attrs []slog.Attr
}

func newBufferHandler(buf *logBuffer, level slog.Level) *bufferHandler {
	return &bufferHandler{buf: buf, level: level}
}

func (h *bufferHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *bufferHandler) Handle(_ context.Context, record slog.Record) error {
	message := record.Message
	for _, attr := range h.attrs {
		message += formatAttr(attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		message += formatAttr(attr)
		return true
	})
	h.buf.add(record.Level, message)
	return nil
}

func (h *bufferHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *bufferHandler) WithGroup(string) slog.Handler {
	return h
}

func formatAttr(attr slog.Attr) string {
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindString {
		return fmt.Sprintf(" %s=%q", attr.Key, value.String())
	}
	return fmt.Sprintf(" %s=%s", attr.Key, value.String())
}
