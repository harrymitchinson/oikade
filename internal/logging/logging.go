// Package logging is what log/slog leaves out: a logger in a context, one that discards, and one
// that hands over each line.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

var discard = slog.New(slog.DiscardHandler)

// Discard returns a logger that writes nothing.
func Discard() *slog.Logger { return discard }

// OrDiscard returns log, or a logger that writes nothing if there's none. A nil *slog.Logger
// panics, so a component whose logger is optional calls this before it first logs.
func OrDiscard(log *slog.Logger) *slog.Logger {
	if log == nil {
		return Discard()
	}
	return log
}

type contextKey struct{}

// NewContext returns ctx carrying log.
func NewContext(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, log)
}

// FromContext returns the logger in ctx, or one that writes nothing.
func FromContext(ctx context.Context) *slog.Logger {
	log, _ := ctx.Value(contextKey{}).(*slog.Logger)
	return OrDiscard(log)
}

// Text returns a logger that writes lines at level or above to w in slog's text format, without
// their time. It's for a home on a fake clock, whose time isn't the wall clock's.
func Text(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// Lines is Text, passing each line to write. Calls to write are serial, and write must not log
// through the same logger.
func Lines(write func(line string), level slog.Level) *slog.Logger {
	return Text(lineWriter(write), level)
}

type lineWriter func(line string)

// Write implements io.Writer. slog's handlers write one record at a time.
func (w lineWriter) Write(p []byte) (int, error) {
	w(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// Test returns a logger for t's log, such as a *testing.T's.
func Test(t interface{ Log(args ...any) }) *slog.Logger {
	return Lines(func(line string) { t.Log(line) }, slog.LevelInfo)
}
