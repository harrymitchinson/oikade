/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package control

import (
	"testing"
	"time"
)

// The scenarios replay the timelines of the walkthrough in #60. Holds start and end with Claim
// and Release, as the runtime will drive them.

func TestScenario3WaterHeaterArbitrated(t *testing.T) {
	s := Settings{Kind: Bool, Default: off, Resolve: On, External: ExternalOverride, OverrideHold: time.Hour}
	h := newHarness(t, s, "22:00")
	h.want("22:00 hold starts", h.claim(Claim{Source: "Rule/water-heater-night", Level: Normal, Value: on}),
		on, Normal, "Rule/water-heater-night", NoLimit)
	h.to("23:00")
	d := h.outside("HomeKit", off)
	h.want("23:00 Home app tap", d, off, Override, "HomeKit", NoLimit)
	if !d.Claims[0].Expires.Equal(at("+1 00:00")) {
		t.Errorf("got expiry %v, want 00:00", d.Claims[0].Expires)
	}
	h.want("00:00 override expires", h.to("+1 00:00"), on, Normal, "Rule/water-heater-night", NoLimit)
	h.restart(s)
	if d := h.to("+1 02:00"); d.Write || d.Value != on {
		t.Errorf("02:00 restart: got %+v, want on with no write", d)
	}
	h.to("+1 06:00")
	h.want("06:00 hold ends", h.release("Rule/water-heater-night"), off, LevelDefault, "", NoLimit)
}

func TestScenario5PumpWithoutBudget(t *testing.T) {
	s := Settings{
		Kind: Bool, Default: off, Resolve: On, External: ExternalOverride, OverrideHold: 2 * time.Hour,
		MinimumOn: time.Hour, MinimumOff: 30 * time.Minute, MaximumStartsPerDay: 4, Location: london,
	}
	h := newHarness(t, s, "00:00")
	// The pump is running from yesterday's 22:00 slot.
	yesterday := day.AddDate(0, 0, -1)
	a, err := New(s, h.clock, State{Known: true, Value: on, LastOn: yesterday.Add(22 * time.Hour), Day: yesterday, Starts: 2})
	if err != nil {
		t.Fatal(err)
	}
	h.a = a

	const budget, frost = "Rule/pump-budget", "Rule/pump-frost"
	if d := h.claim(Claim{Source: budget, Level: Normal, Value: on}); d.Write || d.Value != on {
		t.Errorf("00:00: got %+v, want on with no write", d)
	}
	h.to("01:40")
	h.want("01:40 Home app tap", h.outside("HomeKit", off), off, Override, "HomeKit", NoLimit)
	h.to("02:00")
	h.want("02:00 slot ends", h.release(budget), off, Override, "HomeKit", NoLimit)
	h.to("02:50")
	h.want("02:50 frost", h.claim(Claim{Source: frost, Level: Safety, Value: on}), on, Safety, frost, NoLimit)
	if st := h.a.State(); st.Starts != 1 {
		t.Errorf("02:50: got %d starts today, want 1", st.Starts)
	}
	h.to("03:30")
	d := h.release(frost)
	h.want("03:30 frost ends", d, on, Override, "HomeKit", MinimumOn)
	if !d.Next.Equal(at("03:40")) {
		t.Errorf("03:30: got next %v, want 03:40 when the override expires", d.Next)
	}
	h.want("03:40 override expires", h.to("03:40"), on, LevelDefault, "", MinimumOn)
	h.want("03:50 minimum run done", h.to("03:50"), off, LevelDefault, "", NoLimit)
	h.to("14:00")
	h.want("14:00 next slot", h.claim(Claim{Source: budget, Level: Normal, Value: on}), on, Normal, budget, NoLimit)
	if st := h.a.State(); st.Starts != 2 {
		t.Errorf("14:00: got %d starts today, want 2", st.Starts)
	}
}

func hallLight(t *testing.T, start string) *harness {
	return newHarness(t, Settings{Kind: Bool, Default: off, Resolve: On, External: ExternalOverride, OverrideUntilNext: true}, start)
}

const hallMotion = "Rule/hall-motion"

func TestScenario6HallLight(t *testing.T) {
	t.Run("motion", func(t *testing.T) {
		h := hallLight(t, "20:00")
		h.want("20:00 motion", h.claim(Claim{Source: hallMotion, Level: Normal, Value: on}), on, Normal, hallMotion, NoLimit)
		// The off-delay keeps the hold active from 20:10 to 20:25.
		h.want("20:20", h.to("20:20"), on, Normal, hallMotion, NoLimit)
		h.to("20:25")
		h.want("20:25 off-delay ends", h.release(hallMotion), off, LevelDefault, "", NoLimit)
	})
	t.Run("wall switch off", func(t *testing.T) {
		h := hallLight(t, "20:00")
		h.claim(Claim{Source: hallMotion, Level: Normal, Value: on})
		h.to("20:05")
		h.want("20:05 switch off", h.outside("Device", off), off, Override, "Device", NoLimit)
		// Motion at 20:13 keeps the hold active, which is no change, so the light stays off.
		h.want("20:13 motion", h.to("20:13"), off, Override, "Device", NoLimit)
		h.to("20:25")
		d := h.release(hallMotion)
		h.want("20:25 hold ends", d, off, LevelDefault, "", NoLimit)
		if len(d.Claims) != 0 {
			t.Errorf("20:25: got claims %+v, want the override gone", d.Claims)
		}
		h.to("20:30")
		h.want("20:30 next motion", h.claim(Claim{Source: hallMotion, Level: Normal, Value: on}), on, Normal, hallMotion, NoLimit)
	})
	t.Run("wall switch on with nobody moving", func(t *testing.T) {
		h := hallLight(t, "23:00")
		h.want("23:00 switch on", h.outside("Device", on), on, Override, "Device", NoLimit)
		h.to("23:10")
		d := h.claim(Claim{Source: hallMotion, Level: Normal, Value: on})
		h.want("23:10 motion", d, on, Normal, hallMotion, NoLimit)
		if len(d.Claims) != 1 {
			t.Errorf("23:10: got claims %+v, want only the motion hold", d.Claims)
		}
		h.to("23:20")
		h.want("23:20 hold ends", h.release(hallMotion), off, LevelDefault, "", NoLimit)
	})
}

func TestScenario7TowelRailBoost(t *testing.T) {
	const schedule, boost = "Rule/towel-rail-heating", "Rule/towel-rail-boost"
	s := Settings{Kind: Bool, Default: off, Resolve: On, External: ExternalOverride, OverrideHold: time.Hour}
	t.Run("press at 14:00", func(t *testing.T) {
		h := newHarness(t, s, "14:00")
		h.want("14:00", h.claim(Claim{Source: boost, Level: Normal, Value: on, UntilNext: true}), on, Normal, boost, NoLimit)
		h.to("18:00")
		d := h.claim(Claim{Source: schedule, Level: Normal, Value: on})
		h.want("18:00", d, on, Normal, schedule, NoLimit)
		if len(d.Claims) != 1 {
			t.Errorf("18:00: got claims %+v, want the boost ended", d.Claims)
		}
	})
	t.Run("press at 19:00", func(t *testing.T) {
		h := newHarness(t, s, "18:00")
		h.claim(Claim{Source: schedule, Level: Normal, Value: on})
		h.to("19:00")
		if d := h.claim(Claim{Source: boost, Level: Normal, Value: on, UntilNext: true}); d.Write {
			t.Error("19:00: the boost should be a no-op")
		}
		h.to("21:00")
		h.want("21:00", h.release(schedule), off, LevelDefault, "", NoLimit)
	})
	t.Run("press at 21:30", func(t *testing.T) {
		h := newHarness(t, s, "21:30")
		h.claim(Claim{Source: boost, Level: Normal, Value: on, UntilNext: true})
		h.want("05:59", h.to("+1 05:59"), on, Normal, boost, NoLimit)
		h.to("+1 06:00")
		d := h.claim(Claim{Source: schedule, Level: Normal, Value: on})
		h.want("06:00", d, on, Normal, schedule, NoLimit)
		if len(d.Claims) != 1 {
			t.Errorf("06:00: got claims %+v, want the boost ended", d.Claims)
		}
	})
}

func TestScenario7DoorbellLights(t *testing.T) {
	const doorbell = "Rule/doorbell"
	lightOn := newHarness(t, Settings{Kind: Bool, Default: off, Resolve: On, External: ExternalOverride, OverrideUntilNext: true}, "19:00")
	// Each capability is arbitrated on its own, on the same clock.
	a, err := New(Settings{Kind: Number, Default: 40, Resolve: Highest, External: ExternalOverride, OverrideUntilNext: true, Location: london}, lightOn.clock, State{})
	if err != nil {
		t.Fatal(err)
	}
	level := &harness{t: t, clock: lightOn.clock, a: a}

	expires := at("19:30")
	lightOn.claim(Claim{Source: doorbell, Level: Normal, Value: on, Expires: expires})
	level.want("19:00 level", level.claim(Claim{Source: doorbell, Level: Normal, Value: 100, Expires: expires}), 100, Normal, doorbell, NoLimit)

	lightOn.to("19:10")
	lightOn.want("19:10 switch off", lightOn.outside("Device", off), off, Override, "Device", NoLimit)
	level.want("19:10 level", level.apply(), 100, Normal, doorbell, NoLimit)

	lightOn.to("19:30")
	level.want("19:30 level", level.apply(), 40, LevelDefault, "", NoLimit)
	lightOn.want("19:30 light", lightOn.apply(), off, LevelDefault, "", NoLimit)
}

func TestScenario8LeakShutoff(t *testing.T) {
	s := Settings{Kind: Bool, Default: on, Resolve: Off, External: ExternalRevert}
	const leak = "Rule/leak-shutoff"
	h := newHarness(t, s, "13:00")
	h.want("13:00 open", h.apply(), on, LevelDefault, "", NoLimit)
	h.to("14:00")
	h.want("14:00 leak", h.claim(Claim{Source: leak, Level: Safety, Value: off, Latched: true}), off, Safety, leak, NoLimit)
	h.to("14:05")
	if err := h.a.Outside("HomeKit", on); err != nil {
		t.Fatal(err)
	}
	d := h.a.Decide()
	if !d.Write || d.Value != off {
		t.Fatalf("14:05: got %+v, want a write back to closed", d)
	}
	h.a.Wrote(d.Value)
	if d.Reverted == nil || d.Reverted.Source != "HomeKit" || d.Reverted.Value != on {
		t.Errorf("14:05: got reverted %+v, want HomeKit's open", d.Reverted)
	}
	if !d.Claims[0].Latched {
		t.Error("14:05: the leak claim should show as latched")
	}
	h.want("15:00 sensor dries", h.to("15:00"), off, Safety, leak, NoLimit)
	h.restart(s)
	h.want("15:30 restart", h.to("15:30"), off, Safety, leak, NoLimit)
	h.to("16:00")
	h.want("16:00 reset", h.release(leak), on, LevelDefault, "", NoLimit)
}

func TestScenario9DehumidifierQuietAtNight(t *testing.T) {
	s := Settings{Kind: Bool, Default: off, Resolve: Off, External: ExternalOverride, OverrideHold: time.Hour}
	const humid, night = "Rule/dehumidifier-humid", "Rule/dehumidifier-night"
	h := newHarness(t, s, "21:00")
	h.claim(Claim{Source: humid, Level: Normal, Value: on})
	h.to("22:00")
	d := h.claim(Claim{Source: night, Level: Normal, Value: off})
	h.want("22:00 night", d, off, Normal, night, NoLimit)
	if len(d.Claims) != 2 {
		t.Errorf("22:00: got claims %+v, want both listed", d.Claims)
	}
	h.to("+1 07:00")
	h.want("07:00 morning", h.release(night), on, Normal, humid, NoLimit)
}
