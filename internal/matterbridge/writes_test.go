package matterbridge

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	matterv1alpha1 "github.com/harrymitchinson/oikade/api/matter/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/mqtt"
)

var (
	upstairs = types.NamespacedName{Namespace: "home", Name: "upstairs"}
	deskLamp = types.NamespacedName{Namespace: "home", Name: "desk-lamp"}
	porchFan = types.NamespacedName{Namespace: "home", Name: "porch-fan"}
)

func TestAnnouncerAnnouncesBeforeWriting(t *testing.T) {
	bus := mqtt.NewMemory()
	var announced []string
	a := &Announcer{Bus: bus, Bridge: upstairs, Setter: driver.SetterFunc(func(_ context.Context, dev types.NamespacedName, c string, v any) error {
		for _, m := range bus.Published {
			announced = append(announced, m.Topic+" "+string(m.Payload))
		}
		if dev != deskLamp || c != capability.LightLevel || v != 40.0 {
			t.Errorf("Set(%v, %s, %v)", dev, c, v)
		}
		return nil
	})}
	if err := a.Set(t.Context(), deskLamp, capability.LightLevel, 40.0); err != nil {
		t.Fatal(err)
	}
	want := `oikade/v1/_matter/home/upstairs/writes {"bridge":"upstairs","device":"desk-lamp","capability":"light.level","value":40}`
	if fmt.Sprint(announced) != "["+want+"]" {
		t.Errorf("announced before the write: %v, want %s", announced, want)
	}
	if bus.Published[0].Retained {
		t.Error("the announcement is retained")
	}
}

func TestSubscribeWrites(t *testing.T) {
	bus := mqtt.NewMemory()
	var got []string
	if _, err := SubscribeWrites(t.Context(), bus, func(dev types.NamespacedName, name string, value any) {
		got = append(got, fmt.Sprint(dev, name, value))
	}); err != nil {
		t.Fatal(err)
	}
	a := &Announcer{Bus: bus, Bridge: upstairs, Setter: driver.SetterFunc(func(context.Context, types.NamespacedName, string, any) error { return nil })}
	_ = a.Set(t.Context(), porchFan, capability.SwitchOn, true)
	_ = bus.Publish(t.Context(), Root+"home/upstairs/writes", []byte(`{"bridge":"other","device":"porch-fan","capability":"switch.on","value":true}`), false)
	_ = bus.Publish(t.Context(), Root+"home/upstairs/writes", []byte(`{"bridge":"upstairs","device":"porch-fan","capability":"switch.on","value":3}`), false)
	if want := fmt.Sprint(porchFan, capability.SwitchOn, true); fmt.Sprint(got) != "["+want+"]" {
		t.Errorf("got %v, want only %s", got, want)
	}
}

// recorder is a driver.Setter that records writes.
type recorder struct {
	mu     sync.Mutex
	writes []string
}

func (r *recorder) Set(_ context.Context, dev types.NamespacedName, name string, value any) error {
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
		Current: func(types.NamespacedName, string) (any, bool) { return false, true }}
	c.Start(t.Context())
	for _, v := range []bool{true, false, true, true} {
		c.Add(porchFan, capability.SwitchOn, v)
	}
	c.Add(deskLamp, capability.LightOn, true)
	time.Sleep(150 * time.Millisecond)
	got := rec.list()
	if len(got) != 2 || !contains(got, "porch-fan switch.on=true") || !contains(got, "desk-lamp light.on=true") {
		t.Errorf("writes %v", got)
	}
}

// A burst that ends where the device already is writes nothing.
func TestCoalescerSkipsNoChange(t *testing.T) {
	rec := &recorder{}
	c := &Coalescer{Setter: rec, Window: 30 * time.Millisecond, Timeout: time.Second,
		Current: func(types.NamespacedName, string) (any, bool) { return true, true }}
	c.Start(t.Context())
	c.Add(porchFan, capability.SwitchOn, false)
	c.Add(porchFan, capability.SwitchOn, true)
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
		Current: func(types.NamespacedName, string) (any, bool) { return false, true },
		Now:     func() time.Time { return now }}
	c.Start(t.Context())
	c.Add(porchFan, capability.SwitchOn, true)
	time.Sleep(60 * time.Millisecond)
	c.Add(porchFan, capability.SwitchOn, false) // the device still reports false
	time.Sleep(60 * time.Millisecond)
	c.Add(porchFan, capability.SwitchOn, false) // already sent: skipped
	time.Sleep(60 * time.Millisecond)
	want := []string{"porch-fan switch.on=true", "porch-fan switch.on=false"}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Errorf("writes %v, want %v", got, want)
	}

	// Once the device has had time to report, its state is the baseline again.
	now = now.Add(sentFor)
	c.Add(porchFan, capability.SwitchOn, false)
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
			Current: func(types.NamespacedName, string) (any, bool) { return nil, false }}
		c.Start(t.Context())
		c.Add(deskLamp, capability.LightOn, on)
		c.Add(deskLamp, capability.LightLevel, 40.0)
		time.Sleep(60 * time.Millisecond)
		want := []string{"desk-lamp light.level=40", "desk-lamp light.on=true"}
		if !on {
			want = []string{"desk-lamp light.on=false", "desk-lamp light.level=40"}
		}
		if got := rec.list(); !slices.Equal(got, want) {
			t.Errorf("on=%v: writes %v, want %v", on, got, want)
		}
	}
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

func TestFabricsRoundTrip(t *testing.T) {
	bus := mqtt.NewMemory()
	var got []string
	if _, err := SubscribeFabrics(t.Context(), bus, func(b types.NamespacedName, f []matterv1alpha1.Fabric) {
		got = append(got, fmt.Sprint(b, f))
	}); err != nil {
		t.Fatal(err)
	}
	_ = bus.Publish(t.Context(), FabricsTopic(upstairs), []byte(`[{"fabricIndex":1,"fabricID":"0000000000000001","nodeID":"00000000DEADBEEF","vendorID":4937,"label":"Home"}]`), true)
	_ = bus.Publish(t.Context(), FabricsTopic(upstairs), []byte(`not json`), true)
	want := fmt.Sprint(upstairs, []matterv1alpha1.Fabric{{FabricIndex: 1, FabricID: "0000000000000001", NodeID: "00000000DEADBEEF", VendorID: 4937, Label: "Home"}})
	if fmt.Sprint(got) != "["+want+"]" {
		t.Errorf("got %v", got)
	}
	if !bus.Published[0].Retained {
		t.Error("fabrics aren't retained")
	}
}
