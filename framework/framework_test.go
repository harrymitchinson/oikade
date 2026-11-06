package framework

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
)

func TestCompileRejectsAnUnsoundHome(t *testing.T) {
	when := func(State) bool { return true }
	for name, tc := range map[string]struct {
		home Home
		want string
	}{
		"no name": {Home{Holds: []Hold{{Device: "d", Capability: "switch.on", Value: true, When: when}}}, "needs a name"},
		"twice": {Home{
			Holds:   []Hold{{Name: "a", Device: "d", Capability: "switch.on", Value: true, When: when}},
			Moments: []Moment{{Name: "a", At: []string{"07:00"}, Do: []Action{Wait(time.Second)}}},
		}, "declared twice"},
		"unknown capability": {
			Home{Holds: []Hold{{Name: "a", Device: "d", Capability: "nope", Value: true, When: when}}}, "unknown capability",
		},
		"no trigger": {Home{Moments: []Moment{{Name: "a", Do: []Action{Wait(time.Second)}}}}, "trigger"},
		"nil action": {Home{Moments: []Moment{{Name: "a", At: []string{"07:00"}, Do: []Action{nil}}}}, "no action"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.home.compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

func TestConditionSeesTheHome(t *testing.T) {
	now := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	cond := Condition(func(s State) bool {
		return s.Bool("door", "sensor.contact-open") && s.Number("room", "sensor.temperature") > 20 && s.Now().Equal(now)
	}).toEngine()
	ok, err := cond(engine.Vars{"now": now, "devices": map[string]map[string]any{
		"door": {"sensor.contact-open": true},
		"room": {"sensor.temperature": 21.5},
	}})
	if err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}

// A hold has to keep its state until the devices it reads have reported, so that isn't false.
func TestConditionReadingAnUnreportedValueIsAnError(t *testing.T) {
	cond := Condition(func(s State) bool { return !s.Bool("door", "sensor.contact-open") }).toEngine()
	devices := map[string]map[string]any{"door": {}}
	if _, err := cond(engine.Vars{"devices": devices}); err == nil {
		t.Fatal("got no error")
	}
}

// A device that doesn't exist will never report, so it mustn't pass for one that hasn't yet. The
// engine recovers this panic and logs it.
func TestConditionReadingAnUndeclaredDevicePanics(t *testing.T) {
	cond := Condition(func(s State) bool { return s.Bool("dor", "sensor.contact-open") }).toEngine()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "no device dor") {
			t.Fatalf("got %v", r)
		}
	}()
	_, _ = cond(engine.Vars{"devices": map[string]map[string]any{"door": {}}})
}

func TestDiscoveredAddsADeviceNewSinceTheWelcome(t *testing.T) {
	mirror := hub.NewStore(logr.Discard())
	metas := map[types.NamespacedName]hub.Meta{}
	discovered(mirror, metas, wire.Device{
		Name: "lamp", Room: "hall", Arbitrated: []string{"switch.on"}, Discovered: true,
		Capabilities: []string{"switch.on"}, Values: map[string]any{"switch.on": true},
	})
	snap, ok := mirror.Get(deviceKey("lamp"))
	if !ok || !snap.Discovered || snap.Room != "hall" || !snap.Controls("switch.on") || snap.Values["switch.on"] != true {
		t.Fatalf("got %+v, %v", snap, ok)
	}
	changed(mirror, wire.Message{Device: "lamp", Capability: "switch.on", Value: false})
	if snap, _ := mirror.Get(deviceKey("lamp")); snap.Values["switch.on"] != false {
		t.Fatalf("a later change didn't reach it: %+v", snap)
	}
}
