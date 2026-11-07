package matterbridge

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/ident"
)

var (
	deskLamp = ident.ID{Namespace: "home", Name: "desk-lamp"}
	porchFan = ident.ID{Namespace: "home", Name: "porch-fan"}
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

// A swipe sends a burst of On/Off commands: only the last is written, once.
func TestCoalescerLatestWins(t *testing.T) {
	rec := &recorder{}
	c := &Coalescer{Setter: rec, Window: 30 * time.Millisecond, Timeout: time.Second,
		Current: func(ident.ID, string) (any, bool) { return false, true }}
	c.Start(t.Context())
	for _, v := range []bool{true, false, true, true} {
		c.Add(porchFan, capability.On, v)
	}
	c.Add(deskLamp, capability.On, true)
	time.Sleep(150 * time.Millisecond)
	got := rec.list()
	if len(got) != 2 || !contains(got, "porch-fan on=true") || !contains(got, "desk-lamp on=true") {
		t.Errorf("writes %v", got)
	}
}

// A burst that ends where the device already is writes nothing.
func TestCoalescerSkipsNoChange(t *testing.T) {
	rec := &recorder{}
	c := &Coalescer{Setter: rec, Window: 30 * time.Millisecond, Timeout: time.Second,
		Current: func(ident.ID, string) (any, bool) { return true, true }}
	c.Start(t.Context())
	c.Add(porchFan, capability.On, false)
	c.Add(porchFan, capability.On, true)
	time.Sleep(150 * time.Millisecond)
	if got := rec.list(); len(got) != 0 {
		t.Errorf("writes %v, want none", got)
	}
}

// A burst that ends where the last write left the device writes again, even before the device
// reports that write: the device's stale state isn't the baseline.
func TestCoalescerComparesWithWhatItSent(t *testing.T) {
	rec := &recorder{}
	now := time.Now()
	c := &Coalescer{Setter: rec, Window: 10 * time.Millisecond, Timeout: time.Second,
		Current: func(ident.ID, string) (any, bool) { return false, true },
		Now:     func() time.Time { return now }}
	c.Start(t.Context())
	c.Add(porchFan, capability.On, true)
	time.Sleep(60 * time.Millisecond)
	c.Add(porchFan, capability.On, false) // the device still reports false
	time.Sleep(60 * time.Millisecond)
	c.Add(porchFan, capability.On, false) // already sent: skipped
	time.Sleep(60 * time.Millisecond)
	want := []string{"porch-fan on=true", "porch-fan on=false"}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Errorf("writes %v, want %v", got, want)
	}

	// Once the device has had time to report, its state is the baseline again.
	now = now.Add(sentFor)
	c.Add(porchFan, capability.On, false)
	time.Sleep(60 * time.Millisecond)
	if got := rec.list(); len(got) != 2 {
		t.Errorf("writes %v, want no more", got)
	}
}

// Turning on sends the level first, and turning off sends off first, so a light doesn't flash.
func TestCoalescerOrdersWrites(t *testing.T) {
	for _, on := range []bool{true, false} {
		rec := &recorder{}
		c := &Coalescer{Setter: rec, Window: 10 * time.Millisecond, Timeout: time.Second,
			Current: func(ident.ID, string) (any, bool) { return nil, false }}
		c.Start(t.Context())
		c.Add(deskLamp, capability.On, on)
		c.Add(deskLamp, capability.Brightness, 40.0)
		time.Sleep(60 * time.Millisecond)
		want := []string{"desk-lamp brightness=40", "desk-lamp on=true"}
		if !on {
			want = []string{"desk-lamp on=false", "desk-lamp brightness=40"}
		}
		if got := rec.list(); !slices.Equal(got, want) {
			t.Errorf("on=%v: writes %v, want %v", on, got, want)
		}
	}
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}
