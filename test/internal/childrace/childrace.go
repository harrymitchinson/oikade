// Package childrace fails a test binary when a process it started reported a data race.
//
// test/host and test/cli run built binaries: a host, its home and plugins, and the CLI. Built with
// -race, a child prints its report to its own stderr, which a test reads as just another line, and
// it's often killed rather than left to exit with the race detector's status. So Main points every
// child's reports at files instead, through GORACE, which children inherit, and reads them once the
// tests are done.
package childrace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Main runs the tests, and fails them if a process they started reported a race. Under the race
// detector, GORACE's log_path sends each child's reports to a file of its own, named by its pid.
// The race detector in this process, which read GORACE before Main changed it, is left as it was.
//
// A binary built with -race sleeps for a second as it exits (atexit_sleep_ms, 1000 by default),
// which a host stopping within --stop-timeout=1s can't wait for (OIK-325). Children exit straight
// away instead, as they do without it, unless GORACE already says otherwise.
func Main(m *testing.M) int {
	if !Enabled {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "oikrace")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not make a directory for race reports:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	options := strings.TrimSpace("atexit_sleep_ms=0 " + os.Getenv("GORACE") + " log_path=" + filepath.Join(dir, "race"))
	if err := os.Setenv("GORACE", options); err != nil {
		fmt.Fprintln(os.Stderr, "Could not set GORACE:", err)
		return 1
	}
	code := m.Run()
	reports, err := filepath.Glob(filepath.Join(dir, "race.*"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not list race reports:", err)
		return 1
	}
	if len(reports) == 0 {
		return code
	}
	// One race in a binary every test runs reports once per run, so a few say enough.
	for _, path := range reports[:min(len(reports), shown)] {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Could not read race report:", err)
			return 1
		}
		pid := strings.TrimPrefix(filepath.Ext(path), ".")
		fmt.Fprintf(os.Stderr, "FAIL: process %s, which a test started, reported a data race:\n%s\n", pid, raw)
	}
	if len(reports) > shown {
		fmt.Fprintf(os.Stderr, "FAIL: %d more processes reported a data race\n", len(reports)-shown)
	}
	return 1
}

// shown is how many processes' race reports Main prints.
const shown = 3
