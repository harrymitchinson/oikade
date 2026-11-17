package testkit

import (
	"bytes"
	"sync"
	"testing"
)

func TestUnhexTakesSpacesAndLineBreaks(t *testing.T) {
	if got := Unhex(t, "01 02\n\t0a"); !bytes.Equal(got, []byte{1, 2, 10}) {
		t.Errorf("got % x", got)
	}
}

func TestLocationHasTheZonesOnAnyMachine(t *testing.T) {
	if loc := Location("Europe/London"); loc.String() != "Europe/London" {
		t.Errorf("got %v", loc)
	}
}

// A log can be written from any goroutine while a test counts its lines.
func TestLogsCountsTheLinesWithEveryPart(t *testing.T) {
	l := Logs(t)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { l.Info("Started Plugin", "name", "simswitch") })
	}
	l.Debug("Skipped Device", "name", "porch")
	wg.Wait()
	if n := l.Count(`msg="Started Plugin"`, "name=simswitch"); n != 10 {
		t.Errorf("counted %d, want 10", n)
	}
	if !l.Has("level=DEBUG", "porch") || l.Has("Started Plugin", "porch") {
		t.Errorf("lines = %q", l.Lines())
	}
}

func TestEventuallyReturnsOnceItHolds(t *testing.T) {
	n := 0
	Eventually(t, "the third look", func() bool { n++; return n == 3 })
}
