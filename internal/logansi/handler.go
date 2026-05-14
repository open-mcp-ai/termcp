// Package logansi implements a slog.Handler that prints [LEVEL][time] message key=value …
// with optional ANSI colors (modern terminals: Windows Terminal, PowerShell, bash, zsh; stderr must be a TTY for auto).
package logansi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

const (
	reset     = "\x1b[0m"
	dim       = "\x1b[90m" // field keys
	timeColor = "\x1b[94m" // [timestamp] bright blue
	timeFmt   = "2006-01-02 15:04:05"
)

// Options configures the text handler.
type Options struct {
	MinLevel slog.Level
	Color    bool
}

// Handler is a slog.Handler with bracketed level and time, optional ANSI colors.
type Handler struct {
	opts   Options
	w      io.Writer
	mu     sync.Mutex
	groups []string
	attrs  []slog.Attr
}

// NewTextHandler returns a handler writing to w.
func NewTextHandler(w io.Writer, opts Options) slog.Handler {
	return &Handler{opts: opts, w: w}
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.MinLevel
}

func levelANSI(level slog.Level, color bool) string {
	if !color {
		return ""
	}
	switch {
	case level < slog.LevelInfo:
		return "\x1b[36m"
	case level < slog.LevelWarn:
		return "\x1b[32m"
	case level < slog.LevelError:
		return "\x1b[33m"
	default:
		return "\x1b[31m"
	}
}

func (h *Handler) formatKey(key string) string {
	if len(h.groups) == 0 {
		return key
	}
	var b strings.Builder
	for _, g := range h.groups {
		b.WriteString(g)
		b.WriteByte('.')
	}
	b.WriteString(key)
	return b.String()
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	c := h.opts.Color

	// [LEVEL]
	if c {
		b.WriteString(levelANSI(r.Level, c))
	}
	fmt.Fprintf(&b, "[%s]", strings.ToUpper(r.Level.String()))
	if c {
		b.WriteString(reset)
	}

	// [time]
	if c {
		b.WriteString(timeColor)
	}
	fmt.Fprintf(&b, "[%s]", r.Time.Format(timeFmt))
	if c {
		b.WriteString(reset)
	}

	b.WriteByte(' ')
	b.WriteString(r.Message)

	for _, a := range h.attrs {
		h.appendAttr(&b, a, c)
	}
	r.Attrs(func(a slog.Attr) bool {
		h.appendAttr(&b, a, c)
		return true
	})

	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *Handler) appendAttr(b *strings.Builder, a slog.Attr, color bool) {
	if a.Equal(slog.Attr{}) {
		return
	}
	val := a.Value.Resolve()
	if val.Kind() == slog.KindGroup {
		for _, ga := range val.Group() {
			h.appendAttr(b, slog.Attr{Key: a.Key + "." + ga.Key, Value: ga.Value}, color)
		}
		return
	}
	key := h.formatKey(a.Key)
	b.WriteByte(' ')
	if color {
		b.WriteString(dim)
	}
	b.WriteString(key)
	b.WriteByte('=')
	if color {
		b.WriteString(reset)
	}
	b.WriteString(val.String())
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	cp := *h
	cp.attrs = append(slicesClone(h.attrs), attrs...)
	return &cp
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	cp := *h
	cp.groups = append(slicesClone(h.groups), name)
	return &cp
}

func slicesClone[S ~[]E, E any](s S) S {
	if s == nil {
		return nil
	}
	return append(S(nil), s...)
}
