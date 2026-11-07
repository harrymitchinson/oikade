package clock

import (
	"sync"
	"testing"
	"time"
)

var (
	start = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

	_ Clock = Real{}
	_ Clock = (*Fake)(nil)
)

// fired reports whether c has received, and what.
func fired(c <-chan time.Time) (time.Time, bool) {
	select {
	case t := <-c:
		return t, true
	default:
		return time.Time{}, false
	}
}

func TestFakeNowMovesOnlyWhenTold(t *testing.T) {
	c := NewFake(start)
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
	c.Step(time.Minute)
	if got, want := c.Now(), start.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("after Step, Now() = %v, want %v", got, want)
	}
	later := start.Add(time.Hour)
	c.SetTime(later)
	if got := c.Now(); !got.Equal(later) {
		t.Fatalf("after SetTime, Now() = %v, want %v", got, later)
	}
}

func TestFakeFiresWhenTheClockReachesTheWait(t *testing.T) {
	c := NewFake(start)
	timer := c.NewTimer(time.Minute)
	if !c.HasWaiters() {
		t.Fatal("HasWaiters() = false with a wait pending")
	}

	c.Step(time.Minute - time.Nanosecond)
	if _, ok := fired(timer.C()); ok {
		t.Fatal("fired before its time")
	}
	if !c.HasWaiters() {
		t.Fatal("HasWaiters() = false with a wait still pending")
	}

	c.Step(time.Nanosecond)
	got, ok := fired(timer.C())
	if !ok {
		t.Fatal("didn't fire at its time")
	}
	if want := start.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("received %v, want %v", got, want)
	}
	if c.HasWaiters() {
		t.Fatal("HasWaiters() = true once the wait fired")
	}
}

func TestFakeOneMoveFiresEveryWaitItPasses(t *testing.T) {
	c := NewFake(start)
	first, second, third := c.NewTimer(time.Second), c.NewTimer(time.Minute), c.NewTimer(time.Hour)

	c.SetTime(start.Add(time.Minute))

	if _, ok := fired(first.C()); !ok {
		t.Error("the wait it moved past didn't fire")
	}
	if _, ok := fired(second.C()); !ok {
		t.Error("the wait it moved to didn't fire")
	}
	if _, ok := fired(third.C()); ok {
		t.Error("the wait still ahead fired")
	}
	if !c.HasWaiters() {
		t.Error("HasWaiters() = false with a wait still ahead")
	}
}

// Nothing waits for a receiver, so moving the clock can't hang on a wait nobody listens to.
func TestFakeMovingDoesNotBlockOnAnUnreadWait(t *testing.T) {
	c := NewFake(start)
	c.NewTimer(time.Second)
	c.Step(time.Minute)
	c.Step(time.Minute)
}

func TestFakeWaitOfZeroOrLessFiresOnTheNextMove(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		c := NewFake(start)
		timer := c.NewTimer(d)
		if _, ok := fired(timer.C()); ok {
			t.Fatalf("NewTimer(%v) fired before the clock moved", d)
		}
		c.Step(0)
		if _, ok := fired(timer.C()); !ok {
			t.Fatalf("NewTimer(%v) didn't fire on the next move", d)
		}
	}
}

func TestFakeTimerStop(t *testing.T) {
	c := NewFake(start)
	timer := c.NewTimer(time.Minute)

	timer.Stop()
	if c.HasWaiters() {
		t.Fatal("HasWaiters() = true after Stop")
	}
	c.Step(time.Hour)
	if _, ok := fired(timer.C()); ok {
		t.Fatal("a stopped timer fired")
	}
	timer.Stop() // Stopping it twice is harmless.
}

// A timer that fired has gone, so stopping it leaves its value and every other wait alone.
func TestFakeTimerStopAfterFiring(t *testing.T) {
	c := NewFake(start)
	timer := c.NewTimer(time.Minute)
	other := c.NewTimer(time.Hour)
	c.Step(time.Minute)

	timer.Stop()

	if _, ok := fired(timer.C()); !ok {
		t.Error("Stop lost the time it fired at")
	}
	if !c.HasWaiters() {
		t.Error("Stop removed another wait")
	}
	other.Stop()
}

// Stopping one timer leaves the others waiting.
func TestFakeTimerStopLeavesOtherWaits(t *testing.T) {
	c := NewFake(start)
	before, stopped, after := c.NewTimer(time.Minute), c.NewTimer(time.Minute), c.NewTimer(time.Minute)

	stopped.Stop()
	c.Step(time.Minute)

	if _, ok := fired(before.C()); !ok {
		t.Error("the wait before it didn't fire")
	}
	if _, ok := fired(after.C()); !ok {
		t.Error("the wait after it didn't fire")
	}
}

// The engine and its runs wait on the clock while a simulated home moves it. go test -race checks this.
func TestFakeConcurrentUse(t *testing.T) {
	c := NewFake(start)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				timer := c.NewTimer(time.Second)
				c.Now()
				c.HasWaiters()
				timer.Stop()
			}
		})
	}
	wg.Go(func() {
		for range 100 {
			c.Step(time.Second)
		}
	})
	wg.Wait()
}

func TestRealFires(t *testing.T) {
	c := Real{}
	if d := time.Since(c.Now()); d < 0 || d > time.Minute {
		t.Fatalf("Now() is %v from the system clock", d)
	}
	select {
	case <-c.NewTimer(time.Millisecond).C():
	case <-time.After(10 * time.Second):
		t.Fatal("NewTimer didn't fire")
	}
}

func TestRealTimerStop(t *testing.T) {
	timer := Real{}.NewTimer(time.Millisecond)
	timer.Stop()
	select {
	case <-timer.C():
		t.Fatal("a stopped timer fired")
	case <-time.After(20 * time.Millisecond):
	}
}
