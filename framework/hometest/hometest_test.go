package hometest_test

import (
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/framework"
	"github.com/harrymitchinson/oikade/framework/hometest"
)

const (
	on       = "switch.on"
	humidity = "sensor.relative-humidity"
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
		Name: "fan", Device: "fan", Capability: on, Value: true, OffDelay: 10 * time.Minute,
		When: func(s framework.State) bool { return s.Number("sensor", humidity) > 70 },
	}
}

func TestAHoldFollowsADeviceAndTheClock(t *testing.T) {
	h := hometest.New(t, framework.Home{Holds: []framework.Hold{fanHold()}}, hometest.Options{Devices: devices()})
	h.ExpectValue("fan", on, false)
	h.Set("sensor", humidity, 80.0)
	h.ExpectValue("fan", on, true)
	h.Set("sensor", humidity, 50.0)
	h.Advance(9 * time.Minute)
	h.ExpectValue("fan", on, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue("fan", on, false)
	if got := len(h.Writes()); got != 2 {
		t.Errorf("%d writes, want 2: %v", got, h.Writes())
	}
}

func bedtime() framework.Home {
	return framework.Home{Flows: []framework.Flow{{
		Name: "bedtime", At: []string{"22:30"},
		Run: func(r *framework.FlowRun) error {
			r.Sleep(30 * time.Minute)
			return r.Set(framework.Set{Device: "light", Capability: on, Value: false})
		},
	}}}
}

func TestARestartMidFlowCarriesOn(t *testing.T) {
	h := hometest.New(t, bedtime(), hometest.Options{Devices: devices()})
	h.Advance(22*time.Hour + 40*time.Minute)
	if h.Finished("bedtime") {
		t.Fatal("finished before its wait ended")
	}
	h.ExpectValue("light", on, true)
	h.Restart()
	h.ExpectValue("light", on, true)
	h.Advance(19 * time.Minute)
	h.ExpectValue("light", on, true)
	h.Advance(2 * time.Minute)
	h.ExpectValue("light", on, false)
	h.ExpectFinished("bedtime")
}

func arbitrated() framework.Home {
	return framework.Home{
		Control: []framework.Control{{
			Device: "pump", Capability: on, Default: false, Resolve: framework.PreferOn,
			External: framework.ExternalOverride, OverrideHold: time.Hour,
		}},
		Holds: []framework.Hold{{
			Name: "pump", Device: "pump", Capability: on, Value: true, Priority: framework.Safety,
			Window: &framework.Window{Start: "01:00", End: "02:00"},
		}},
	}
}

func TestClaimsAreArbitrated(t *testing.T) {
	h := hometest.New(t, arbitrated(), hometest.Options{Devices: devices()})
	h.Advance(90 * time.Minute)
	h.ExpectValue("pump", on, true)
	h.ExpectClaim("pump", on, "Rule/pump", "Safety")
	// A tap in an app loses to a Safety claim.
	h.Set("pump", on, false)
	h.ExpectClaim("pump", on, "Rule/pump", "Safety")
	h.Advance(time.Hour)
	h.ExpectValue("pump", on, false)
	if got := h.Claims("pump", on); len(got) != 0 {
		t.Errorf("claims %+v after the window", got)
	}
}

func TestABudgetRunsItsMinutes(t *testing.T) {
	home := framework.Home{
		Control: []framework.Control{{
			Device: "pump", Capability: on, Default: false, Resolve: framework.PreferOn,
			External: framework.ExternalOverride, OverrideHold: time.Hour,
		}},
		Holds: []framework.Hold{{
			Name: "pump", Device: "pump", Capability: on, Value: true,
			Budget: &framework.Budget{Minutes: func(framework.State) int { return 60 }},
		}},
	}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.ExpectValue("pump", on, true)
	h.Restart() // A deploy mustn't add to the day's run.
	h.Advance(61 * time.Minute)
	h.ExpectValue("pump", on, false)
	h.Advance(20 * time.Hour)
	h.ExpectValue("pump", on, false)
	if got := len(h.Writes()); got != 2 {
		t.Errorf("%d writes, want on and off: %v", got, h.Writes())
	}
}

func TestAMomentWaits(t *testing.T) {
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{{Device: "sensor", Capability: humidity}},
		Do: []framework.Action{
			framework.Set{Device: "fan", Capability: on, Value: true},
			framework.Wait(5 * time.Minute),
			framework.Set{Device: "fan", Capability: on, Value: false},
		},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set("sensor", humidity, 41.0)
	h.ExpectValue("fan", on, true)
	h.Advance(6 * time.Minute)
	h.ExpectValue("fan", on, false)
	h.ExpectFinished("porch")
}

func TestAWaitEndsWhenItSaysInsideALongerMove(t *testing.T) {
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{{Device: "sensor", Capability: humidity}},
		Do: []framework.Action{
			framework.Wait(90 * time.Second),
			framework.Set{Device: "fan", Capability: on, Value: true},
		},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	start := h.Now()
	h.Set("sensor", humidity, 41.0)
	h.Advance(time.Hour)
	writes := h.Writes()
	if len(writes) != 1 || !writes[0].Time.Equal(start.Add(90*time.Second)) {
		t.Fatalf("writes %v, want one at %s", writes, start.Add(90*time.Second))
	}
}

func TestARunInProgressIsntFinished(t *testing.T) {
	home := framework.Home{Moments: []framework.Moment{{
		Name: "porch", On: []framework.Change{{Device: "sensor", Capability: humidity}},
		Do: []framework.Action{framework.Wait(5 * time.Minute)},
	}}}
	h := hometest.New(t, home, hometest.Options{Devices: devices()})
	h.Set("sensor", humidity, 41.0)
	h.Advance(6 * time.Minute)
	h.ExpectFinished("porch")
	h.Set("sensor", humidity, 42.0)
	if h.Finished("porch") {
		t.Fatal("finished while its second run waits")
	}
	h.Advance(6 * time.Minute)
	h.ExpectFinished("porch")
}
