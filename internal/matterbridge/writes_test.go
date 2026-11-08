package matterbridge

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/ident"
)

var (
	deskLamp = ident.ID{Name: "desk-lamp"}
	porchFan = ident.ID{Name: "porch-fan"}
)

// recorder is a driver.Setter that records writes.
type recorder struct {
	mu     sync.Mutex
	writes []string
}

func (r *recorder) Set(_ context.Context, dev ident.ID, name string, value any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, fmt.Sprint(dev.Name, " ", name, "=", value))
	return nil
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.writes...)
}

// window is how long writes gather in these tests. The clock is fake, so it is only a number.
const window = 50 * time.Millisecond

// coalescer returns a started Coalescer on a fake clock, whose device reports current.
func coalescer(t *testing.T, current func(ident.ID, string) (any, bool)) (*Coalescer, *recorder, *clock.Fake) {
	t.Helper()
	rec := &recorder{}
	clk := clock.NewFake(time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC))
	c := &Coalescer{Setter: rec, Current: current, Window: window, Timeout: time.Second, Clock: clk}
	c.Start(t.Context())
	return c, rec, clk
}

// burstEnds moves the clock past the window and waits for every burst to be sent.
func burstEnds(c *Coalescer, clk *clock.Fake) {
	clk.Step(window)
	c.flushes.Wait()
}

func reports(v any) func(ident.ID, string) (any, bool) {
	return func(ident.ID, string) (any, bool) { return v, true }
}

// A swipe sends a burst of On/Off commands: only the last is written, once.
func TestCoalescerLatestWins(t *testing.T) {
	c, rec, clk := coalescer(t, reports(false))
	for _, v := range []bool{true, false, true, true} {
		c.Add(porchFan, capability.On, v)
	}
	c.Add(deskLamp, capability.On, true)
	clk.Step(window - time.Millisecond)
	if got := rec.list(); len(got) != 0 {
		t.Errorf("wrote %v before the burst was over", got)
	}
	burstEnds(c, clk)
	got := rec.list()
	if len(got) != 2 || !contains(got, "porch-fan on=true") || !contains(got, "desk-lamp on=true") {
		t.Errorf("writes %v", got)
	}
}

// A burst that ends where the device already is writes nothing.
func TestCoalescerSkipsNoChange(t *testing.T) {
	c, rec, clk := coalescer(t, reports(true))
	c.Add(porchFan, capability.On, false)
	c.Add(porchFan, capability.On, true)
	burstEnds(c, clk)
	if got := rec.list(); len(got) != 0 {
		t.Errorf("writes %v, want none", got)
	}
}

// A burst that ends where the last write left the device writes again, even before the device
// reports that write: the device's stale state isn't the baseline.
func TestCoalescerComparesWithWhatItSent(t *testing.T) {
	c, rec, clk := coalescer(t, reports(false))
	c.Add(porchFan, capability.On, true)
	burstEnds(c, clk)
	c.Add(porchFan, capability.On, false) // the device still reports false
	burstEnds(c, clk)
	c.Add(porchFan, capability.On, false) // already sent: skipped
	burstEnds(c, clk)
	want := []string{"porch-fan on=true", "porch-fan on=false"}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Errorf("writes %v, want %v", got, want)
	}

	// Once the device has had time to report, its state is the baseline again.
	clk.Step(sentFor)
	c.Add(porchFan, capability.On, false)
	burstEnds(c, clk)
	if got := rec.list(); len(got) != 2 {
		t.Errorf("writes %v, want no more", got)
	}
}

// A report means the device has spoken, so its own state is the baseline again at once.
func TestCoalescerReportEndsWhatWasSent(t *testing.T) {
	c, rec, clk := coalescer(t, reports(false))
	c.Add(porchFan, capability.On, true)
	burstEnds(c, clk)
	c.Reported(porchFan, capability.On) // it reports false: something turned it off
	c.Add(porchFan, capability.On, true)
	burstEnds(c, clk)
	want := []string{"porch-fan on=true", "porch-fan on=true"}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Errorf("writes %v, want %v", got, want)
	}
}

// Turning on sends the level first, and turning off sends off first, so a light doesn't flash.
func TestCoalescerOrdersWrites(t *testing.T) {
	for _, on := range []bool{true, false} {
		c, rec, clk := coalescer(t, func(ident.ID, string) (any, bool) { return nil, false })
		c.Add(deskLamp, capability.On, on)
		c.Add(deskLamp, capability.Brightness, 40.0)
		burstEnds(c, clk)
		want := []string{"desk-lamp brightness=40", "desk-lamp on=true"}
		if !on {
			want = []string{"desk-lamp on=false", "desk-lamp brightness=40"}
		}
		if got := rec.list(); !slices.Equal(got, want) {
			t.Errorf("on=%v: writes %v, want %v", on, got, want)
		}
	}
}

// slowDevice is a Setter whose first write hangs until the test lets it go, and which notes any
// write that started while another was still going.
type slowDevice struct {
	recorder
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
	going   int
	overlap bool
}

func (d *slowDevice) Set(ctx context.Context, dev ident.ID, name string, value any) error {
	d.mu.Lock()
	d.calls++
	first := d.calls == 1
	d.going++
	d.overlap = d.overlap || d.going > 1
	d.mu.Unlock()
	if first {
		close(d.started)
		<-d.release
	}
	err := d.recorder.Set(ctx, dev, name, value)
	d.mu.Lock()
	d.going--
	d.mu.Unlock()
	return err
}

// A burst that ends while the last is still being sent waits for it, and gathers writes meanwhile,
// so the device ends on the latest of them however slow it is.
func TestCoalescerSendsADevicesBurstsInOrder(t *testing.T) {
	dev := &slowDevice{started: make(chan struct{}), release: make(chan struct{})}
	clk := clock.NewFake(time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC))
	ended := make(chan struct{}, 2)
	c := &Coalescer{Setter: dev, Current: reports(nil), Window: window, Timeout: time.Second, Clock: clk,
		ended: func(ident.ID) { ended <- struct{}{} }}
	c.Start(t.Context())
	c.Add(deskLamp, capability.On, true)
	clk.Step(window)
	<-dev.started
	<-ended
	c.Add(deskLamp, capability.On, false)
	clk.Step(window)
	<-ended // the second burst is over, and the first is still being sent
	c.Add(deskLamp, capability.Brightness, 40.0)
	c.Add(deskLamp, capability.On, true)
	close(dev.release)
	clk.Step(window) // ends any burst that started anew, so a regression fails rather than hangs
	c.flushes.Wait()
	if dev.overlap {
		t.Error("a burst was sent while the last was still being sent")
	}
	want := []string{"desk-lamp on=true", "desk-lamp brightness=40"}
	if got := dev.list(); !slices.Equal(got, want) {
		t.Errorf("writes %v, want %v", got, want)
	}
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}
