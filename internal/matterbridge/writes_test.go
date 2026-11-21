package matterbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/clock"
	"oikade.dev/internal/control"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/testkit"
)

var (
	deskLamp    = ident.ID{Name: "desk-lamp"}
	porchFan    = ident.ID{Name: "porch-fan"}
	officeBlind = ident.ID{Name: "office-blind"}
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

// A capability that travels is written even where it last reported: a blind on its way reports
// where it is, and StopMotion writes that position to stop it there.
func TestCoalescerWritesATravellersLastReportedPosition(t *testing.T) {
	c, rec, clk := coalescer(t, reports(40.0))
	c.Add(officeBlind, capability.Position, 40.0)
	clk.Step(travelWindow)
	c.flushes.Wait()
	if got := rec.list(); !slices.Equal(got, []string{"office-blind position=40"}) {
		t.Errorf("writes %v", got)
	}
}

// The Home app sends a slider's move as it goes, 1% and then 18% a second later for one drag of a
// shutter: the shutter gets one write, the last, once the drag has stopped for the travel window.
func TestCoalescerSendsOnlyADragsLastPosition(t *testing.T) {
	c, rec, clk := coalescer(t, reports(0.0))
	c.Add(officeBlind, capability.Position, 1.0)
	clk.Step(time.Second)
	c.Add(officeBlind, capability.Position, 18.0)
	clk.Step(travelWindow - time.Millisecond)
	if got := rec.list(); len(got) != 0 {
		t.Errorf("wrote %v before the drag had stopped for the travel window", got)
	}
	clk.Step(time.Millisecond)
	c.flushes.Wait()
	if got := rec.list(); !slices.Equal(got, []string{"office-blind position=18"}) {
		t.Errorf("writes %v, want only the last position", got)
	}
}

// A capability that doesn't travel keeps the short window, even in a burst a travelling one has
// joined: the burst ends when the last window does.
func TestCoalescerGathersEachWriteForItsWindow(t *testing.T) {
	c, rec, clk := coalescer(t, reports(nil))
	c.Add(porchFan, capability.On, true)
	burstEnds(c, clk)
	if got := rec.list(); !slices.Equal(got, []string{"porch-fan on=true"}) {
		t.Errorf("writes %v after the short window", got)
	}
	c.Add(officeBlind, capability.On, true)
	c.Add(officeBlind, capability.Position, 60.0)
	clk.Step(window)
	if got := rec.list(); len(got) != 1 {
		t.Errorf("writes %v before the position's window was over", got)
	}
	clk.Step(travelWindow - window)
	c.flushes.Wait()
	want := []string{"porch-fan on=true", "office-blind position=60", "office-blind on=true"}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Errorf("writes %v, want %v", got, want)
	}
}

// A stop ends the burst at once: a shutter travelling for the window would pass where it stops.
func TestCoalescerSendsAtOnceWhenAsked(t *testing.T) {
	c, rec, _ := coalescer(t, reports(30.0))
	c.Add(officeBlind, capability.Position, 70.0)
	c.Add(officeBlind, capability.Position, 40.0)
	c.Send(officeBlind)
	testkit.Eventually(t, "the burst's write", func() bool { return len(rec.list()) > 0 })
	c.flushes.Wait()
	if got := rec.list(); !slices.Equal(got, []string{"office-blind position=40"}) {
		t.Errorf("writes %v, want the stop's position at once", got)
	}
	c.Send(officeBlind) // nothing gathering: nothing to send
	if c.Busy() {
		t.Error("busy with nothing gathering")
	}
}

// A write after Send joins the burst without holding it back for another window: a second stop, or
// another controller's move, can't make a stop wait.
func TestCoalescerSendIsntHeldBackByALaterWrite(t *testing.T) {
	c, rec, _ := coalescer(t, reports(30.0))
	// The burst waits here, once Send has ended it and before it takes its writes, so the later
	// write lands while it's being sent rather than after.
	ended, sendIt := make(chan struct{}), make(chan struct{})
	c.ended = func(ident.ID) {
		close(ended)
		<-sendIt
	}
	c.Add(officeBlind, capability.Position, 70.0)
	c.Send(officeBlind)
	<-ended // over without the clock moving: Send ended it
	c.Add(officeBlind, capability.Position, 40.0)
	close(sendIt)
	c.flushes.Wait()
	if got := rec.list(); !slices.Equal(got, []string{"office-blind position=40"}) {
		t.Errorf("writes %v, want the last position, at once", got)
	}
}

// As the host stops, every burst still gathering is sent at once.
func TestCoalescerSendAllEndsEveryBurst(t *testing.T) {
	c, rec, _ := coalescer(t, reports(nil))
	c.Add(officeBlind, capability.Position, 70.0)
	c.Add(porchFan, capability.On, true)
	if !c.Gathering(officeBlind, capability.Position) {
		t.Error("a write waiting for its window isn't gathering")
	}
	c.SendAll()
	testkit.Eventually(t, "both writes", func() bool { return len(rec.list()) == 2 })
	c.flushes.Wait()
	if c.Gathering(officeBlind, capability.Position) {
		t.Error("a sent write is still gathering")
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

// A write a claim refused is the rule working, so it's logged at Info with the claim that won, and
// a write that fails is still an error.
func TestCoalescerLogsARefusalAsInfo(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	log := logging.Lines(func(line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
	}, slog.LevelInfo)
	clk := clock.NewFake(time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC))
	c := &Coalescer{
		Setter: driver.SetterFunc(func(_ context.Context, dev ident.ID, _ string, _ any) error {
			if dev == deskLamp {
				return &driver.Refused{Holder: "Rule/hall-too-humid", Level: control.NameSafety}
			}
			return errors.New("timed out")
		}),
		Current: reports(false), Window: window, Timeout: time.Second, Clock: clk,
	}
	c.Start(logging.NewContext(t.Context(), log))
	c.Add(deskLamp, capability.On, true)
	c.Add(porchFan, capability.On, true)
	burstEnds(c, clk)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("logged %q", lines)
	}
	refused, failed := lines[0], lines[1]
	if strings.Contains(failed, "desk-lamp") {
		refused, failed = failed, refused
	}
	for _, want := range []string{"level=INFO", `msg="Refused Matter write"`, "device=desk-lamp", "capability=on",
		"value=true", "claim=Rule/hall-too-humid", "claimLevel=" + control.NameSafety} {
		if !strings.Contains(refused, want) {
			t.Errorf("%q doesn't say %s", refused, want)
		}
	}
	if !strings.Contains(failed, "level=ERROR") || !strings.Contains(failed, "porch-fan") {
		t.Errorf("a failed write logged %q", failed)
	}
}

func TestCoalescerIsBusyUntilABurstEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &Coalescer{Window: time.Hour}
	c.Start(ctx)
	if c.Busy() {
		t.Error("busy before any write")
	}
	c.Add(ident.ID{Name: "lamp"}, "on", true)
	if !c.Busy() {
		t.Error("not busy with a write gathering")
	}
	cancel()
	c.flushes.Wait()
	if c.Busy() {
		t.Error("still busy after the burst ended")
	}
}
