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

func TestFakeNextWait(t *testing.T) {
	c := NewFake(start)
	if _, ok := c.NextWait(); ok {
		t.Fatal("NextWait() found a wait on a clock nothing waits on")
	}
	hour, second := c.NewTimer(time.Hour), c.NewTimer(time.Second)
	c.NewTimer(time.Minute)
	if got, ok := c.NextWait(); !ok || !got.Equal(start.Add(time.Second)) {
		t.Fatalf("NextWait() = %v, %t, want the second's wait", got, ok)
	}
	second.Stop()
	if got, _ := c.NextWait(); !got.Equal(start.Add(time.Minute)) {
		t.Fatalf("NextWait() = %v after stopping the soonest, want the minute's wait", got)
	}
	c.Step(time.Minute)
	if got, _ := c.NextWait(); !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("NextWait() = %v after the minute fired, want the hour's wait", got)
	}
	hour.Stop()
	if _, ok := c.NextWait(); ok {
		t.Fatal("NextWait() found a wait once every one was stopped or fired")
	}
}

func TestAfterFuncOnAFake(t *testing.T) {
	c := NewFake(start)
	ran := make(chan time.Time, 1)
	AfterFunc(c, time.Minute, func() { ran <- c.Now() })
	stopped := AfterFunc(c, time.Minute, func() { t.Error("a stopped AfterFunc ran") })
	stopped.Stop()
	stopped.Stop() // Stopping it twice is harmless.

	c.Step(time.Minute - time.Nanosecond)
	select {
	case <-ran:
		t.Fatal("ran before its time")
	case <-time.After(20 * time.Millisecond):
	}
	c.Step(time.Nanosecond)
	select {
	case at := <-ran:
		if want := start.Add(time.Minute); !at.Equal(want) {
			t.Fatalf("ran at %v, want %v", at, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never ran")
	}
	if c.HasWaiters() {
		t.Fatal("HasWaiters() = true once both had fired or stopped")
	}
}

func TestAfterFuncOnTheRealClock(t *testing.T) {
	ran := make(chan struct{})
	AfterFunc(Real{}, time.Millisecond, func() { close(ran) })
	AfterFunc(Real{}, time.Millisecond, func() { t.Error("a stopped AfterFunc ran") }).Stop()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("never ran")
	}
	time.Sleep(10 * time.Millisecond) // for the stopped one, had it not stopped
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
				c.NextWait()
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
