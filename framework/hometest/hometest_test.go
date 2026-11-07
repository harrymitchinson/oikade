package hometest_test

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/framework"
	"github.com/harrymitchinson/oikade/framework/hometest"
)

const (
	on       = string(framework.On)
	humidity = string(framework.Humidity)
)

// The devices' handles, as oikade types would write them.
var (
	sensor = framework.Handle[float64]{Device: "sensor", Capability: framework.Humidity}
	fan    = framework.Handle[bool]{Device: "fan", Capability: framework.On}
	pump   = framework.Handle[bool]{Device: "pump", Capability: framework.On}
	light  = framework.Handle[bool]{Device: "light", Capability: framework.On}
)

func devices() []hometest.Device {
	return []hometest.Device{
		{Name: "sensor", Capabilities: []string{humidity}, Values: map[string]any{humidity: 40.0}},
		{Name: "fan", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "pump", Capabilities: []string{on}, Values: map[string]any{on: false}},
		{Name: "light", Capabilities: []string{on}, Values: map[string]any{on: true}},
	}
}

func fanHold() framework.Hold {
	return framework.Hold{
		Name: "fan", Set: fan.To(true), OffDelay: 10 * time.Minute,
		When: func(s framework.State) bool { return s.Get(sensor) > 70 },
	}
}

func TestAHoldFollowsADeviceAndTheClock(t *testing.T) {
	h := hometest.New(t, framework.Home{Holds: []framework.Hold{fanHold()}}, hometest.Options{Devices: devices()})
	h.ExpectValue(fan, false)
	h.Set(sensor, 80.0)
	h.ExpectValue(fan, true)
	h.Set(sensor, 50.0)
	h.Advance(9 * time.Minute)
	h.ExpectValue(fan, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(fan, false)
	if got := len(h.Writes()); got != 2 {
		t.Errorf("%d writes, want 2: %v", got, h.Writes())
	}
}

func bedtime() framework.Home {
	return framework.Home{Flows: []framework.Flow{{
		Name: "bedtime", At: []string{"22:30"},
		Run: func(r *framework.FlowRun) error {
			r.Sleep(30 * time.Minute)
			return r.Set(light.To(false))
		},
	}}}
}

func TestARestartMidFlowCarriesOn(t *testing.T) {
	h := hometest.New(t, bedtime(), hometest.Options{Devices: devices()})
	h.Advance(22*time.Hour + 40*time.Minute)
	if h.Finished("bedtime") {
		t.Fatal("finished before its wait ended")
	}
	h.ExpectValue(light, true)
	h.Restart()
	h.ExpectValue(light, true)
	h.Advance(19 * time.Minute)
	h.ExpectValue(light, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue(light, false)
	h.ExpectFinished("bedtime")
}

func arbitrated() framework.Home {
	return framework.Home{
		Control: []framework.Control{{
			Default: pump.To(false), Resolve: framework.PreferOn,
			External: framework.ExternalOverride, OverrideHold: time.Hour,
		}},
		Holds: []framework.Hold{{
			Name: "pump", Set: pump.To(true), Priority: framework.Safety,
			Window: &framework.Window{Start: "01:00", End: "02:00"},
		}},
	}
}

func TestClaimsAreArbitrated(t *testing.T) {
	h := hometest.New(t, arbitrated(), hometest.Options{Devices: devices()})
	h.Advance(90 * time.Minute)
	h.ExpectValue(pump, true)
	h.ExpectClaim(pump, "Rule/pump", "Safety")
	// A tap in an app loses to a Safety claim.
	h.Set(pump, false)
	h.ExpectClaim(pump, "Rule/pump", "Safety")
	h.Advance(time.Hour)
	h.ExpectValue(pump, false)
	if got := h.Claims(pump); len(got) != 0 {
		t.Errorf("claims %+v after the window", got)
	}
}

func TestABudgetRunsItsMinutes(t *testing.T) {
	home := framework.Home{
		Control: []framework.Control{{
			Default: pump.To(false), Resolve: framework.PreferOn,
			External: framework.ExternalOverride, OverrideHold: time.Hour,
		}},
		Holds: []framework.Hold{{
			Name: "pump", Set: pump.To(true),
			Budget: &framework.Budget{Minutes: func(framework.State) int { return 60 }},
		}},
	}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.ExpectValue(pump, true)
	h.Restart() // A deploy mustn't add to the day's run.
	h.Advance(61 * time.Minute)
	h.ExpectValue(pump, false)
	h.Advance(20 * time.Hour)
	h.ExpectValue(pump, false)
	if got := len(h.Writes()); got != 2 {
		t.Errorf("%d writes, want on and off: %v", got, h.Writes())
	}
}

func TestAMomentWaits(t *testing.T) {
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{sensor},
		Do: []framework.Action{
			fan.To(true),
			framework.Wait(5 * time.Minute),
			fan.To(false),
		},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set(sensor, 41.0)
	h.ExpectValue(fan, true)
	h.Advance(6 * time.Minute)
	h.ExpectValue(fan, false)
	h.ExpectFinished("porch")
}

func TestAWaitEndsWhenItSaysInsideALongerMove(t *testing.T) {
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{sensor},
		Do: []framework.Action{
			framework.Wait(90 * time.Second),
			fan.To(true),
		},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	start := h.Now()
	h.Set(sensor, 41.0)
	h.Advance(time.Hour)
	writes := h.Writes()
	if len(writes) != 1 || !writes[0].Time.Equal(start.Add(90*time.Second)) {
		t.Fatalf("writes %v, want one at %s", writes, start.Add(90*time.Second))
	}
}

func TestARunInProgressIsntFinished(t *testing.T) {
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{sensor},
		Do: []framework.Action{framework.Wait(5 * time.Minute)},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set(sensor, 41.0)
	h.Advance(6 * time.Minute)
	h.ExpectFinished("porch")
	h.Set(sensor, 42.0)
	if h.Finished("porch") {
		t.Fatal("finished while its second run waits")
	}
	h.Advance(6 * time.Minute)
	h.ExpectFinished("porch")
}

// "Living room off" limited to lights turns off the lamps and leaves the fridge's plug, in the same
// room, on.
func TestARoomWriteLimitedToLightsLeavesThePlugOn(t *testing.T) {
	brightness := string(framework.Brightness)
	lamp := framework.Handle[bool]{Device: "floor-lamp", Capability: framework.On}
	ceiling := framework.Handle[bool]{Device: "ceiling-light", Capability: framework.On}
	fridge := framework.Handle[bool]{Device: "fridge-plug", Capability: framework.On}
	home := framework.Home{Moments: []framework.Moment{{
		Name: "living-room-off", At: []string{"23:00"},
		Do: []framework.Action{
			framework.InRoom("living-room", framework.On).Types(framework.OnOffLight, framework.DimmableLight).To(false),
		},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: []hometest.Device{
		{
			Name: "floor-lamp", Room: "living-room", Capabilities: []string{on, brightness},
			Values: map[string]any{on: true, brightness: 80.0},
		},
		{
			Name: "ceiling-light", Room: "living-room", Capabilities: []string{on}, DeviceType: framework.OnOffLight,
			Values: map[string]any{on: true},
		},
		{Name: "fridge-plug", Room: "living-room", Capabilities: []string{on}, Values: map[string]any{on: true}},
	}})
	h.Advance(23*time.Hour + time.Minute)
	h.ExpectValue(lamp, false)
	h.ExpectValue(ceiling, false)
	h.ExpectValue(fridge, true)
}
