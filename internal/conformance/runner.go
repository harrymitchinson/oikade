package conformance

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Run checks the plugin cfg describes outside go test, writing each check's result to w. It
// returns whether every check passed.
func Run(cfg Config, w io.Writer) bool {
	top := &runT{name: "suite"}
	var s *Suite
	top.do(func() { s = New(top, cfg) })
	if s != nil {
		for _, c := range s.Checks() {
			ct := &runT{name: c.Name}
			start := time.Now()
			ct.do(func() { c.Run(ct) })
			ct.cleanup()
			ct.report(w, time.Since(start))
			if ct.Failed() {
				top.fail()
			}
		}
	}
	// Cleanups log the plugin's output when anything failed.
	top.cleanup()
	if top.Failed() || len(top.logs) > 0 {
		top.report(w, 0)
	}
	if top.Failed() {
		_, _ = fmt.Fprintln(w, "FAIL")
		return false
	}
	_, _ = fmt.Fprintln(w, "PASS")
	return true
}

// runT is a T for Run. Like testing.T, Fatal stops only the check that calls it.
type runT struct {
	name string

	mu       sync.Mutex
	failed   bool
	logs     []string
	cleanups []func()
}

// do runs f on its own goroutine, so a Fatal can end it with runtime.Goexit.
func (t *runT) do(f func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	<-done
}

func (t *runT) cleanup() {
	for {
		t.mu.Lock()
		n := len(t.cleanups)
		if n == 0 {
			t.mu.Unlock()
			return
		}
		f := t.cleanups[n-1]
		t.cleanups = t.cleanups[:n-1]
		t.mu.Unlock()
		t.do(f)
	}
}

func (t *runT) report(w io.Writer, took time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := "PASS"
	if t.failed {
		result = "FAIL"
	}
	_, _ = fmt.Fprintf(w, "--- %s: %s (%.2fs)\n", result, t.name, took.Seconds())
	for _, l := range t.logs {
		_, _ = fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(l, "\n", "\n    "))
	}
}

func (t *runT) log(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logs = append(t.logs, s)
}

func (t *runT) fail() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failed = true
}

func (t *runT) Helper() {}

func (t *runT) Cleanup(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanups = append(t.cleanups, f)
}

func (t *runT) Failed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failed
}

func (t *runT) Log(args ...any)                 { t.log(strings.TrimSuffix(fmt.Sprintln(args...), "\n")) }
func (t *runT) Logf(format string, args ...any) { t.log(fmt.Sprintf(format, args...)) }
func (t *runT) Error(args ...any)               { t.Log(args...); t.fail() }
func (t *runT) Errorf(format string, args ...any) {
	t.Logf(format, args...)
	t.fail()
}
func (t *runT) Fatal(args ...any)                 { t.Error(args...); runtime.Goexit() }
func (t *runT) Fatalf(format string, args ...any) { t.Errorf(format, args...); runtime.Goexit() }
