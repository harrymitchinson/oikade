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
	for name, wait := range map[string]func(*Fake, time.Duration) <-chan time.Time{
		"After":    (*Fake).After,
		"NewTimer": func(c *Fake, d time.Duration) <-chan time.Time { return c.NewTimer(d).C() },
	} {
		t.Run(name, func(t *testing.T) {
			c := NewFake(start)
			ch := wait(c, time.Minute)
			if !c.HasWaiters() {
				t.Fatal("HasWaiters() = false with a wait pending")
			}

			c.Step(time.Minute - time.Nanosecond)
			if _, ok := fired(ch); ok {
				t.Fatal("fired before its time")
			}
			if !c.HasWaiters() {
				t.Fatal("HasWaiters() = false with a wait still pending")
			}

			c.Step(time.Nanosecond)
			got, ok := fired(ch)
			if !ok {
				t.Fatal("didn't fire at its time")
			}
			if want := start.Add(time.Minute); !got.Equal(want) {
				t.Fatalf("received %v, want %v", got, want)
			}
			if c.HasWaiters() {
				t.Fatal("HasWaiters() = true once the wait fired")
			}
		})
	}
}

func TestFakeOneMoveFiresEveryWaitItPasses(t *testing.T) {
	c := NewFake(start)
	first, second, third := c.After(time.Second), c.After(time.Minute), c.After(time.Hour)

	c.SetTime(start.Add(time.Minute))

	if _, ok := fired(first); !ok {
		t.Error("the wait it moved past didn't fire")
	}
	if _, ok := fired(second); !ok {
		t.Error("the wait it moved to didn't fire")
	}
	if _, ok := fired(third); ok {
		t.Error("the wait still ahead fired")
	}
	if !c.HasWaiters() {
		t.Error("HasWaiters() = false with a wait still ahead")
	}
}

// Nothing waits for a receiver, so moving the clock can't hang on a wait nobody listens to.
func TestFakeMovingDoesNotBlockOnAnUnreadWait(t *testing.T) {
	c := NewFake(start)
	c.After(time.Second)
	c.NewTimer(time.Second)
	c.Step(time.Minute)
	c.Step(time.Minute)
}

func TestFakeWaitOfZeroOrLessFiresOnTheNextMove(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		c := NewFake(start)
		ch := c.After(d)
		if _, ok := fired(ch); ok {
			t.Fatalf("After(%v) fired before the clock moved", d)
		}
		c.Step(0)
		if _, ok := fired(ch); !ok {
			t.Fatalf("After(%v) didn't fire on the next move", d)
		}
	}
}

func TestFakeTimerStop(t *testing.T) {
	c := NewFake(start)
	timer := c.NewTimer(time.Minute)

	if !timer.Stop() {
		t.Fatal("Stop() = false for a pending timer")
	}
	if c.HasWaiters() {
		t.Fatal("HasWaiters() = true after Stop")
	}
	c.Step(time.Hour)
	if _, ok := fired(timer.C()); ok {
		t.Fatal("a stopped timer fired")
	}
	if timer.Stop() {
		t.Fatal("Stop() = true for a timer already stopped")
	}
}

func TestFakeTimerStopAfterFiring(t *testing.T) {
	c := NewFake(start)
	timer := c.NewTimer(time.Minute)
	c.Step(time.Minute)
	if timer.Stop() {
		t.Fatal("Stop() = true for a timer that fired")
	}
}

// Stopping one timer leaves the others waiting.
func TestFakeTimerStopLeavesOtherWaits(t *testing.T) {
	c := NewFake(start)
	before, stopped, after := c.After(time.Minute), c.NewTimer(time.Minute), c.After(time.Minute)

	stopped.Stop()
	c.Step(time.Minute)

	if _, ok := fired(before); !ok {
		t.Error("the wait before it didn't fire")
	}
	if _, ok := fired(after); !ok {
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
				c.After(time.Second)
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
	case <-c.After(time.Millisecond):
	case <-time.After(10 * time.Second):
		t.Fatal("After didn't fire")
	}
	select {
	case <-c.NewTimer(time.Millisecond).C():
	case <-time.After(10 * time.Second):
		t.Fatal("NewTimer didn't fire")
	}
}

func TestRealTimerStop(t *testing.T) {
	timer := Real{}.NewTimer(time.Hour)
	if !timer.Stop() {
		t.Fatal("Stop() = false for a pending timer")
	}
	if timer.Stop() {
		t.Fatal("Stop() = true for a timer already stopped")
	}
}
