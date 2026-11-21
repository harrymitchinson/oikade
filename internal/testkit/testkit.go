// Package testkit is what more than one package's tests need and plain testing leaves out: waiting
// for a condition, a time zone that's there on any machine, bytes written as hex, a log a test
// can read, and golden files. It holds nothing else: no assertions, which stay plain testing.
package testkit

import (
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	// Location needs the zones on a machine that has none, as a slim container doesn't.
	_ "time/tzdata"

	"oikade.dev/internal/logging"
)

const (
	// limit bounds a wait: far longer than any test's work takes, even under -race on a busy machine.
	limit = 10 * time.Second
	// interval is how often a wait looks, which is soon enough that a test waits about as long as
	// the work, and seldom enough not to take a core from it.
	interval = 100 * time.Microsecond
)

// Eventually returns once cond holds, and fails t if it doesn't within 10 seconds. what says what
// it waited for, as in "timed out waiting for <what>".
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(interval)
	}
}

// Location is the IANA time zone name, for a package-level var. It panics on a name that isn't
// one.
func Location(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// Unhex is the bytes s writes in hex, which may have spaces and line breaks between them, and
// fails t if it isn't hex.
func Unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("%q isn't hex: %v", s, err)
	}
	return b
}

// Log is a logger that keeps its lines, at Debug and above, in slog's text format without the time.
// Any goroutine can log to it while the test reads it.
type Log struct {
	*slog.Logger
	mu    sync.Mutex
	lines []string
}

// Logs is a Log for t. If t fails, its lines go to t's log, so the failure shows what was logged.
func Logs(t testing.TB) *Log {
	t.Helper()
	l := &Log{}
	l.Logger = logging.Lines(func(line string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, line)
	}, slog.LevelDebug)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("The log:\n%s", strings.Join(l.Lines(), "\n"))
		}
	})
	return l
}

// Lines are the lines logged so far, oldest first.
func (l *Log) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// Count is how many lines have every one of parts in them.
func (l *Log) Count(parts ...string) int {
	n := 0
	for _, line := range l.Lines() {
		if containsAll(line, parts) {
			n++
		}
	}
	return n
}

// Has says whether a line has every one of parts in it.
func (l *Log) Has(parts ...string) bool { return l.Count(parts...) > 0 }

func containsAll(line string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(line, p) {
			return false
		}
	}
	return true
}
