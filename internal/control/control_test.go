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
	"fmt"
	"strings"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
)

var london = mustLocation("Europe/London")

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// day is 2026-10-21 in London.
var day = time.Date(2026, time.October, 21, 0, 0, 0, 0, london)

// at is HH:MM on day, or on following days with "+1 06:00".
func at(s string) time.Time {
	var plus, h, m int
	if strings.HasPrefix(s, "+") {
		if _, err := fmt.Sscanf(s, "+%d %d:%d", &plus, &h, &m); err != nil {
			panic(err)
		}
	} else if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		panic(err)
	}
	return day.AddDate(0, 0, plus).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

const (
	off = 0.0
	on  = 1.0
)

type harness struct {
	t     *testing.T
	clock *clocktesting.FakeClock
	a     *Arbiter
}

func newHarness(t *testing.T, s Settings, start string) *harness {
	t.Helper()
	if s.Location == nil {
		s.Location = london
	}
	c := clocktesting.NewFakeClock(at(start))
	a, err := New(s, c, State{})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, clock: c, a: a}
}

// to moves the clock to s and applies the decision, as the runtime would.
func (h *harness) to(s string) Decision {
	h.t.Helper()
	h.clock.SetTime(at(s))
	return h.apply()
}

func (h *harness) apply() Decision {
	h.t.Helper()
	d := h.a.Decide()
	if d.Write {
		h.a.Wrote(d.Value)
	}
	return d
}

func (h *harness) claim(c Claim) Decision {
	h.t.Helper()
	if err := h.a.Claim(c); err != nil {
		h.t.Fatal(err)
	}
	return h.apply()
}

func (h *harness) release(source string) Decision {
	h.t.Helper()
	h.a.Release(source)
	return h.apply()
}

func (h *harness) outside(source string, v float64) Decision {
	h.t.Helper()
	if err := h.a.Outside(source, v); err != nil {
		h.t.Fatal(err)
	}
	return h.apply()
}

// restart replaces the arbiter with one restored from its state, as after a runtime restart.
func (h *harness) restart(s Settings) {
	h.t.Helper()
	if s.Location == nil {
		s.Location = london
	}
	a, err := New(s, h.clock, h.a.State())
	if err != nil {
		h.t.Fatal(err)
	}
	h.a = a
}

func (h *harness) want(when string, d Decision, value float64, level Level, source string, limit Limit) {
	h.t.Helper()
	if d.Value != value || d.Level != level || d.Source != source || d.Limit != limit {
		h.t.Errorf("%s: got value %v, level %v, source %q, limit %v; want %v, %v, %q, %v",
			when, d.Value, d.Level, d.Source, d.Limit, value, level, source, limit)
	}
}

var boolSettings = Settings{Kind: Bool, Resolve: On, External: ExternalOverride, OverrideHold: time.Hour}

func TestValidate(t *testing.T) {
	for name, s := range map[string]Settings{
		"bool with Highest":     {Kind: Bool, Resolve: Highest, External: ExternalRevert},
		"bool default 2":        {Kind: Bool, Default: 2, Resolve: On, External: ExternalRevert},
		"number with On":        {Kind: Number, Resolve: On, External: ExternalRevert},
		"no kind":               {Resolve: On, External: ExternalRevert},
		"no external":           {Kind: Bool, Resolve: On},
		"override without hold": {Kind: Bool, Resolve: On, External: ExternalOverride},
		"negative minimum":      {Kind: Bool, Resolve: On, External: ExternalRevert, MinimumOn: -time.Minute},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate passed", name)
		}
	}
	for name, s := range map[string]Settings{
		"bool":          boolSettings,
		"number":        {Kind: Number, Default: 20, Resolve: Lowest, External: ExternalRevert},
		"override next": {Kind: Bool, Resolve: Off, External: ExternalOverride, OverrideUntilNext: true},
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClaimRejectsBadInput(t *testing.T) {
	h := newHarness(t, boolSettings, "10:00")
	for name, c := range map[string]Claim{
		"no source":     {Level: Normal, Value: on},
		"default level": {Source: "Rule/a", Level: LevelDefault, Value: on},
		"bool value 2":  {Source: "Rule/a", Level: Normal, Value: 2},
	} {
		if err := h.a.Claim(c); err == nil {
			t.Errorf("%s: Claim passed", name)
		}
	}
	if err := h.a.Outside("", on); err == nil {
		t.Error("Outside passed without a source")
	}
}

func TestLevels(t *testing.T) {
	h := newHarness(t, boolSettings, "10:00")
	h.want("no claims", h.apply(), off, LevelDefault, "", NoLimit)
	h.want("normal", h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on}), on, Normal, "Rule/n", NoLimit)
	h.want("override", h.claim(Claim{Source: "Rule/o", Level: Override, Value: off}), off, Override, "Rule/o", NoLimit)
	h.want("safety", h.claim(Claim{Source: "Rule/s", Level: Safety, Value: on}), on, Safety, "Rule/s", NoLimit)
	h.want("safety released", h.release("Rule/s"), off, Override, "Rule/o", NoLimit)
	h.want("override released", h.release("Rule/o"), on, Normal, "Rule/n", NoLimit)
}

func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		kind    Kind
		resolve Resolve
		values  []float64
		want    float64
	}{
		{Bool, On, []float64{off, on}, on},
		{Bool, Off, []float64{on, off}, off},
		{Number, Highest, []float64{40, 80, 60}, 80},
		{Number, Lowest, []float64{40, 80, 20}, 20},
	} {
		h := newHarness(t, Settings{Kind: tc.kind, Resolve: tc.resolve, External: ExternalRevert}, "10:00")
		for i, v := range tc.values {
			h.claim(Claim{Source: fmt.Sprintf("Rule/%d", i), Level: Normal, Value: v})
		}
		if d := h.apply(); d.Value != tc.want || d.Level != Normal {
			t.Errorf("resolve %v over %v: got %v at %v, want %v", tc.resolve, tc.values, d.Value, d.Level, tc.want)
		}
	}
}

func TestClaimFromSameSourceReplaces(t *testing.T) {
	h := newHarness(t, boolSettings, "10:00")
	h.claim(Claim{Source: "Rule/a", Level: Safety, Value: on})
	d := h.claim(Claim{Source: "Rule/a", Level: Normal, Value: off})
	h.want("replaced", d, off, Normal, "Rule/a", NoLimit)
	if len(d.Claims) != 1 {
		t.Errorf("got %d claims, want 1", len(d.Claims))
	}
}

func TestClaimsSortedHighestFirst(t *testing.T) {
	h := newHarness(t, boolSettings, "10:00")
	h.claim(Claim{Source: "Rule/b", Level: Normal, Value: on})
	h.claim(Claim{Source: "Rule/s", Level: Safety, Value: on})
	d := h.claim(Claim{Source: "Rule/a", Level: Normal, Value: off})
	got := make([]string, 0, len(d.Claims))
	for _, c := range d.Claims {
		got = append(got, c.Source)
	}
	if want := "Rule/s Rule/a Rule/b"; strings.Join(got, " ") != want {
		t.Errorf("got %v, want %s", got, want)
	}
}

func TestMinimumOnHoldsSafetyBack(t *testing.T) {
	s := boolSettings
	s.MinimumOn = time.Hour
	h := newHarness(t, s, "10:00")
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	d := h.claim(Claim{Source: "Rule/leak", Level: Safety, Value: off})
	h.want("10:00", d, on, Safety, "Rule/leak", MinimumOn)
	if d.Wanted != off || !d.Next.Equal(at("11:00")) {
		t.Errorf("got wanted %v, next %v; want off at 11:00", d.Wanted, d.Next)
	}
	h.want("10:59", h.to("10:59"), on, Safety, "Rule/leak", MinimumOn)
	h.want("11:00", h.to("11:00"), off, Safety, "Rule/leak", NoLimit)
}

func TestMinimumOffHoldsSafetyBack(t *testing.T) {
	s := boolSettings
	s.MinimumOff = 30 * time.Minute
	h := newHarness(t, s, "10:00")
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	h.to("10:10")
	h.release("Rule/n")
	d := h.claim(Claim{Source: "Rule/frost", Level: Safety, Value: on})
	h.want("10:10", d, off, Safety, "Rule/frost", MinimumOff)
	if !d.Next.Equal(at("10:40")) {
		t.Errorf("got next %v, want 10:40", d.Next)
	}
	h.want("10:40", h.to("10:40"), on, Safety, "Rule/frost", NoLimit)
}

func TestMaximumOnRestsForMinimumOff(t *testing.T) {
	s := boolSettings
	s.MaximumOn, s.MinimumOff = 2*time.Hour, 30*time.Minute
	h := newHarness(t, s, "10:00")
	h.claim(Claim{Source: "Rule/s", Level: Safety, Value: on})
	if d := h.apply(); !d.Next.Equal(at("12:00")) {
		t.Errorf("got next %v, want 12:00", d.Next)
	}
	h.want("12:00", h.to("12:00"), off, Safety, "Rule/s", MaximumOn)
	h.want("12:29", h.to("12:29"), off, Safety, "Rule/s", MinimumOff)
	h.want("12:30", h.to("12:30"), on, Safety, "Rule/s", NoLimit)
	h.want("14:30", h.to("14:30"), off, Safety, "Rule/s", MaximumOn)
}

func TestMaximumOnWithoutMinimumOffWaitsForOff(t *testing.T) {
	s := boolSettings
	s.MaximumOn = 2 * time.Hour
	h := newHarness(t, s, "10:00")
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	h.want("12:00", h.to("12:00"), off, Normal, "Rule/n", MaximumOn)
	h.want("15:00", h.to("15:00"), off, Normal, "Rule/n", MaximumOn)
	h.want("released", h.release("Rule/n"), off, LevelDefault, "", NoLimit)
	h.want("claimed again", h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on}), on, Normal, "Rule/n", NoLimit)
}

func TestMaximumStartsPerDay(t *testing.T) {
	s := boolSettings
	s.MaximumStartsPerDay = 2
	h := newHarness(t, s, "10:00")
	for _, tm := range []string{"10:00", "11:00"} {
		h.to(tm)
		h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
		h.release("Rule/n")
	}
	h.to("12:00")
	d := h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	h.want("third start", d, off, Normal, "Rule/n", MaximumStartsPerDay)
	if !d.Next.Equal(at("+1 00:00")) {
		t.Errorf("got next %v, want midnight", d.Next)
	}
	h.want("midnight", h.to("+1 00:00"), on, Normal, "Rule/n", NoLimit)
}

func TestOutsideChangeOverridesForHold(t *testing.T) {
	h := newHarness(t, boolSettings, "10:00")
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	h.to("10:30")
	d := h.outside("HomeKit", off)
	h.want("10:30", d, off, Override, "HomeKit", NoLimit)
	if c := d.Claims[0]; !c.External || !c.Expires.Equal(at("11:30")) {
		t.Errorf("got claim %+v, want an external claim expiring at 11:30", c)
	}
	h.want("11:30", h.to("11:30"), on, Normal, "Rule/n", NoLimit)
}

func TestOutsideChangeReplacesEarlierOutsideChange(t *testing.T) {
	h := newHarness(t, boolSettings, "10:00")
	h.outside("Device", off)
	h.claim(Claim{Source: "Rule/o", Level: Override, Value: off})
	d := h.outside("HomeKit", on)
	h.want("latest outside change", d, on, Override, "HomeKit", NoLimit)
	if len(d.Claims) != 2 {
		t.Errorf("got %d claims, want the rule's and HomeKit's", len(d.Claims))
	}
}

func TestOutsideChangeOverridesUntilNext(t *testing.T) {
	s := boolSettings
	s.OverrideHold, s.OverrideUntilNext = 0, true
	h := newHarness(t, s, "10:00")
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	h.want("tap off", h.outside("Device", off), off, Override, "Device", NoLimit)
	h.want("later", h.to("18:00"), off, Override, "Device", NoLimit)
	h.want("hold ends", h.release("Rule/n"), off, LevelDefault, "", NoLimit)
	h.want("hold starts again", h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on}), on, Normal, "Rule/n", NoLimit)
}

func TestOutsideChangeIsNotFoughtByLimits(t *testing.T) {
	s := boolSettings
	s.MinimumOn, s.MinimumOff = time.Hour, time.Hour
	h := newHarness(t, s, "10:00")
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	h.to("10:10")
	h.want("off inside minimumOn", h.outside("Device", off), off, Override, "Device", NoLimit)
	h.to("10:20")
	h.want("on inside minimumOff", h.outside("Device", on), on, Override, "Device", NoLimit)
	if st := h.a.State(); st.Starts != 2 || !st.LastOff.Equal(at("10:10")) {
		t.Errorf("got %d starts, last off %v; want 2 and 10:10", st.Starts, st.LastOff)
	}
}

func TestRevertWritesWinnerBack(t *testing.T) {
	// Counting HomeKit's change would make 10:05 the last start, so minimumOn would keep the
	// valve open, and the reopening at 10:30 would be the third start of the day.
	s := Settings{Kind: Bool, Default: on, Resolve: Off, External: ExternalRevert, MinimumOn: time.Hour, MaximumStartsPerDay: 2}
	h := newHarness(t, s, "09:00")
	h.apply()
	h.to("10:00")
	h.claim(Claim{Source: "Rule/leak", Level: Safety, Value: off})
	before := h.a.State()
	h.to("10:05")
	if err := h.a.Outside("HomeKit", on); err != nil {
		t.Fatal(err)
	}
	d := h.a.Decide()
	if !d.Write || d.Value != off || d.Limit != NoLimit {
		t.Fatalf("got %+v; want a write back to off past minimumOn", d)
	}
	if d.Reverted == nil || d.Reverted.Source != "HomeKit" || d.Reverted.Value != on || !d.Reverted.At.Equal(at("10:05")) {
		t.Errorf("got reverted %+v, want HomeKit's on at 10:05", d.Reverted)
	}
	h.a.Wrote(d.Value)
	if st := h.a.State(); st.Starts != before.Starts || !st.LastOn.Equal(before.LastOn) || !st.LastOff.Equal(before.LastOff) || st.Reverting != nil {
		t.Errorf("got state %+v; a reverted change should leave the counters as %+v", st, before)
	}
	h.to("10:30")
	if d := h.release("Rule/leak"); d.Value != on || d.Limit != NoLimit {
		t.Errorf("got %+v; the default should be written, and the reverted change isn't a start", d)
	}
}

func TestRevertOfAChangeThatAgreesIsAccepted(t *testing.T) {
	s := Settings{Kind: Bool, Resolve: On, External: ExternalRevert}
	h := newHarness(t, s, "10:00")
	h.apply()
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	h.to("10:05")
	h.outside("Device", off) // reverted
	h.to("10:06")
	h.release("Rule/n")
	h.to("10:07")
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	if err := h.a.Outside("Device", on); err != nil {
		t.Fatal(err)
	}
	if d := h.a.Decide(); d.Write || d.Value != on {
		t.Errorf("got %+v; a change that agrees with the winner needs no write", d)
	}
}

func TestClaimEnds(t *testing.T) {
	t.Run("for", func(t *testing.T) {
		h := newHarness(t, boolSettings, "10:00")
		d := h.claim(Claim{Source: "Rule/door", Level: Normal, Value: on, Expires: at("10:30")})
		if !d.Next.Equal(at("10:30")) {
			t.Errorf("got next %v, want 10:30", d.Next)
		}
		h.want("10:29", h.to("10:29"), on, Normal, "Rule/door", NoLimit)
		h.want("10:30", h.to("10:30"), off, LevelDefault, "", NoLimit)
	})
	t.Run("until Next", func(t *testing.T) {
		h := newHarness(t, boolSettings, "10:00")
		h.claim(Claim{Source: "Rule/boost", Level: Normal, Value: on, UntilNext: true})
		h.want("much later", h.to("+2 10:00"), on, Normal, "Rule/boost", NoLimit)
		h.claim(Claim{Source: "Rule/sched", Level: Normal, Value: on})
		h.want("sched off", h.release("Rule/sched"), off, LevelDefault, "", NoLimit)
	})
	t.Run("until Next ends when a higher level arrives", func(t *testing.T) {
		h := newHarness(t, boolSettings, "10:00")
		h.claim(Claim{Source: "Rule/boost", Level: Normal, Value: on, UntilNext: true})
		h.claim(Claim{Source: "Rule/s", Level: Safety, Value: on})
		d := h.release("Rule/s")
		h.want("safety gone", d, off, LevelDefault, "", NoLimit)
	})
	t.Run("for and until Next, for first", func(t *testing.T) {
		h := newHarness(t, boolSettings, "21:30")
		h.claim(Claim{Source: "Rule/boost", Level: Normal, Value: on, UntilNext: true, Expires: at("22:30")})
		h.want("22:30", h.to("22:30"), off, LevelDefault, "", NoLimit)
	})
	t.Run("for and until Next, next first", func(t *testing.T) {
		h := newHarness(t, boolSettings, "17:30")
		h.claim(Claim{Source: "Rule/boost", Level: Normal, Value: on, UntilNext: true, Expires: at("18:30")})
		h.to("18:00")
		h.claim(Claim{Source: "Rule/sched", Level: Normal, Value: on})
		d := h.apply()
		if len(d.Claims) != 1 || d.Claims[0].Source != "Rule/sched" {
			t.Errorf("got claims %+v, want only the schedule", d.Claims)
		}
	})
	t.Run("until Released", func(t *testing.T) {
		h := newHarness(t, boolSettings, "10:00")
		h.claim(Claim{Source: "Rule/lock", Level: Override, Value: on})
		h.want("days later", h.to("+3 10:00"), on, Override, "Rule/lock", NoLimit)
		h.want("released", h.release("Rule/lock"), off, LevelDefault, "", NoLimit)
	})
}

func TestStateSurvivesRestart(t *testing.T) {
	s := boolSettings
	s.MinimumOn, s.OverrideHold = time.Hour, 2*time.Hour
	h := newHarness(t, s, "10:00")
	h.claim(Claim{Source: "Rule/boost", Level: Normal, Value: on, UntilNext: true})
	h.claim(Claim{Source: "Rule/sched", Level: Normal, Value: off})
	h.to("10:20")
	h.outside("HomeKit", off)
	h.restart(s)
	h.want("after restart", h.to("10:30"), off, Override, "HomeKit", NoLimit)
	h.want("override ends", h.to("12:20"), on, Normal, "Rule/boost", NoLimit)
	h.restart(s)
	h.claim(Claim{Source: "Rule/sched", Level: Normal, Value: on})
	if d := h.apply(); len(d.Claims) != 1 {
		t.Errorf("got %+v; the boost's baseline should survive the restart and end it", d.Claims)
	}
}

func TestStateIsACopy(t *testing.T) {
	h := newHarness(t, boolSettings, "10:00")
	h.claim(Claim{Source: "Rule/n", Level: Normal, Value: on})
	st := h.a.State()
	st.Claims[0].Value = off
	if d := h.apply(); d.Value != on {
		t.Error("changing a State changed the arbiter")
	}
}
