// Package clock is the time Oikade runs on: the real one in the host, and a fake one that a simulated
// home and the tests move by hand.
package clock

import (
	"slices"
	"sync"
	"time"
)

// Clock tells the time and waits for it.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a wait that can be stopped. A wait that ends early has to stop its timer: on a Fake, a timer
// left running still counts as something waiting on the clock.
type Timer interface {
	// C receives the time when the timer fires.
	C() <-chan time.Time
	Stop()
}

// Real is the system clock.
type Real struct{}

// Now returns the current time.
func (Real) Now() time.Time { return time.Now() }

// NewTimer returns a timer that fires once d has passed.
func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop()               { t.timer.Stop() }

// Fake is a clock that only moves when it's told to. A wait fires when the clock is moved to or past
// its time, so a wait of zero or less fires on the next move, not at once.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeTimer
}

// NewFake returns a fake clock set to t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now returns the fake clock's time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer returns a timer that fires once the clock has moved on by d.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, at: f.now.Add(d), c: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, t)
	return t
}

// Step moves the clock on by d.
func (f *Fake) Step(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set(f.now.Add(d))
}

// SetTime moves the clock to t.
func (f *Fake) SetTime(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set(t)
}

// set moves the clock to now and fires what was waiting for it. Each channel has room for its one
// send, so this never blocks with the lock held.
func (f *Fake) set(now time.Time) {
	f.now = now
	f.waiters = slices.DeleteFunc(f.waiters, func(t *fakeTimer) bool {
		if t.at.After(now) {
			return false
		}
		t.c <- now
		return true
	})
}

// HasWaiters reports whether anything is waiting on the clock.
func (f *Fake) HasWaiters() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters) > 0
}

type fakeTimer struct {
	clock *Fake
	at    time.Time
	c     chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if i := slices.Index(t.clock.waiters, t); i >= 0 {
		t.clock.waiters = slices.Delete(t.clock.waiters, i, i+1)
	}
}
