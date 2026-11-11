package host

import (
	"context"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

func change(value any) api.Event {
	return api.Event{Type: api.EventChange, Device: "lamp", Capability: "brightness", Value: value}
}

// answer runs awaitAnswer on a write of brightness 80, and sends it reports.
func answer(t *testing.T, times settleTimes, has func() bool, reports ...any) (<-chan bool, chan<- api.Event) {
	t.Helper()
	events := make(chan api.Event)
	done := make(chan bool, 1)
	go func() {
		done <- awaitAnswer(context.Background(), events, "lamp", "brightness", 80.0, has, times)
	}()
	for _, r := range reports {
		select {
		case events <- change(r):
		case <-done:
			t.Fatalf("stopped listening before the report of %v", r)
		}
	}
	return done, events
}

func never() bool { return false }

// A lamp mid-fade reports brightness on its way to 80: that isn't its answer, 80 is.
func TestSetWaitsThroughAFadeForTheValueAsked(t *testing.T) {
	times := settleTimes{wait: time.Minute, quiet: 20 * time.Millisecond}
	done, _ := answer(t, times, never, 35.0, 60.0, 80.0)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("80 wasn't taken as the answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after 80")
	}
}

// A report of another value isn't an answer, so the wait runs out.
func TestSetMidFadeHasntAnswered(t *testing.T) {
	times := settleTimes{wait: 50 * time.Millisecond, quiet: 10 * time.Millisecond}
	done, _ := answer(t, times, never, 35.0)
	if <-done {
		t.Fatal("35 was taken as the answer to 80")
	}
}

// A driver that rounds reports 80.2 for a write of 80: that's its answer.
func TestSetTakesARoundedReportAsTheAnswer(t *testing.T) {
	times := settleTimes{wait: time.Minute, quiet: 10 * time.Millisecond}
	done, _ := answer(t, times, never, 80.2)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("80.2 wasn't taken as the answer to 80")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after 80.2")
	}
}

// One step further away than rounding is another value, as from a fade.
func TestSetDoesntTakeANearbyValueAsTheAnswer(t *testing.T) {
	times := settleTimes{wait: 50 * time.Millisecond, quiet: 10 * time.Millisecond}
	done, _ := answer(t, times, never, 79.0)
	if <-done {
		t.Fatal("79 was taken as the answer to 80")
	}
}

// ExternalRevert writes the old value back after the device answers: set waits to hear it, and then
// for quiet again, so that what it prints is the value written back.
func TestSetWaitsForARevert(t *testing.T) {
	const quiet = 200 * time.Millisecond
	times := settleTimes{wait: time.Minute, quiet: quiet}
	done, events := answer(t, times, never, 80.0)
	time.Sleep(quiet / 2)
	select {
	case events <- change(20.0):
	case <-done:
		t.Fatal("stopped listening before the write-back")
	}
	reverted := time.Now()
	if !<-done {
		t.Fatal("the answer was lost")
	}
	if waited := time.Since(reverted); waited < quiet*3/4 {
		t.Fatalf("waited %v after the write-back, want %v", waited, quiet)
	}
}

// A device that already has the value sends nothing, and has answered.
func TestSetToTheValueAlreadyThereHasAnswered(t *testing.T) {
	times := settleTimes{wait: time.Minute, quiet: 10 * time.Millisecond}
	done, _ := answer(t, times, func() bool { return true })
	if !<-done {
		t.Fatal("not answered")
	}
}
