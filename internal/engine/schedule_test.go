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

package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/sun"
)

var (
	london     = mustLocation("Europe/London")
	londonSite = &sun.Site{Latitude: 51.5074, Longitude: -0.1278}
)

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func local(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04 MST", s, london)
	if err != nil {
		panic(err)
	}
	return t
}

func atTrigger(hhmm string, days ...homev1alpha1.Weekday) homev1alpha1.Trigger {
	return homev1alpha1.Trigger{At: &homev1alpha1.AtTrigger{Time: hhmm, Days: days}}
}

func sunsetTrigger(offset time.Duration) homev1alpha1.Trigger {
	return homev1alpha1.Trigger{Sun: &homev1alpha1.SunTrigger{Event: homev1alpha1.SunEventSunset, Offset: &metav1.Duration{Duration: offset}}}
}

func compiled(t *testing.T, triggers ...homev1alpha1.Trigger) *Compiled {
	t.Helper()
	c, err := Compile(rule("timed", homev1alpha1.RuleSpec{Triggers: triggers, Do: []homev1alpha1.Action{delay(time.Second)}}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// follow returns the next n fires after t.
func follow(c *Compiled, t time.Time, n int, site *sun.Site) []time.Time {
	var out []time.Time
	for range n {
		f, ok := c.nextFire(t, london, site)
		if !ok {
			break
		}
		out = append(out, f.At)
		t = f.At
	}
	return out
}

func expectTimes(t *testing.T, got []time.Time, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if w := local(want[i]); !got[i].Equal(w) {
			t.Errorf("fire %d: got %v, want %v", i, got[i].In(london), w)
		}
	}
}

func TestAtAcrossDaylightSaving(t *testing.T) {
	t.Run("an evening time keeps its local time", func(t *testing.T) {
		got := follow(compiled(t, atTrigger("18:00")), local("2026-10-24 12:00 BST"), 2, nil)
		expectTimes(t, got, "2026-10-24 18:00 BST", "2026-10-25 18:00 GMT")
	})
	t.Run("a skipped time fires when the clocks go forward", func(t *testing.T) {
		got := follow(compiled(t, atTrigger("01:30")), local("2026-03-28 12:00 GMT"), 2, nil)
		expectTimes(t, got, "2026-03-29 02:00 BST", "2026-03-30 01:30 BST")
	})
	t.Run("a repeated time fires the first time only", func(t *testing.T) {
		got := follow(compiled(t, atTrigger("01:30")), local("2026-10-24 12:00 BST"), 2, nil)
		expectTimes(t, got, "2026-10-25 01:30 BST", "2026-10-26 01:30 GMT")
	})
}

func TestDaysAreTheDayTheTriggerFires(t *testing.T) {
	// Friday and Saturday nights at 00:30 fire on Saturday and Sunday.
	got := follow(compiled(t, atTrigger("00:30", "Sat", "Sun")), local("2026-10-23 12:00 BST"), 3, nil)
	expectTimes(t, got, "2026-10-24 00:30 BST", "2026-10-25 00:30 BST", "2026-10-31 00:30 GMT")
}

func TestSunFollowsTheDay(t *testing.T) {
	c := compiled(t, sunsetTrigger(-15*time.Minute))
	got := follow(c, local("2026-10-24 12:00 BST"), 2, londonSite)
	for i, day := range []int{24, 25} {
		set, _ := londonSite.Time(sun.Sunset, 2026, time.October, day, london)
		if want := set.Add(-15 * time.Minute); len(got) <= i || !got[i].Equal(want) {
			t.Errorf("fire %d: got %v, want %v", i, got, want)
		}
	}
	if _, ok := c.nextFire(local("2026-10-24 12:00 BST"), london, nil); ok {
		t.Error("a sun trigger fired without a site")
	}
}

func TestCompileTimeTriggers(t *testing.T) {
	for name, tc := range map[string]struct {
		trigger homev1alpha1.Trigger
		wantErr string
	}{
		"two kinds": {homev1alpha1.Trigger{
			At:     &homev1alpha1.AtTrigger{Time: "07:00"},
			Change: &homev1alpha1.ChangeTrigger{Device: "m", Capability: capability.Occupancy},
		}, "exactly one of change, at or sun"},
		"none":          {homev1alpha1.Trigger{}, "exactly one of change, at or sun"},
		"short time":    {atTrigger("7:00"), "is not HH:MM"},
		"bad time":      {atTrigger("24:00"), "is not HH:MM"},
		"bad day":       {atTrigger("07:00", "Monday"), "unknown day"},
		"long offset":   {sunsetTrigger(7 * time.Hour), "between -6h and 6h"},
		"unknown event": {homev1alpha1.Trigger{Sun: &homev1alpha1.SunTrigger{Event: "Noon"}}, "unknown event"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Compile(rule("timed", homev1alpha1.RuleSpec{
				Triggers: []homev1alpha1.Trigger{tc.trigger}, Do: []homev1alpha1.Action{delay(time.Second)},
			}))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("got %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

type schedules struct {
	mu  sync.Mutex
	got []Schedule
}

func (s *schedules) record(_ types.NamespacedName, sched Schedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, sched)
}

func (s *schedules) last() Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return Schedule{}
	}
	return s.got[len(s.got)-1]
}

// timedHarness runs a Rule named lights-on that turns the hallway light on at its triggers.
func timedHarness(t *testing.T, now time.Time, site *sun.Site, edit func(*homev1alpha1.Rule), triggers ...homev1alpha1.Trigger) (*harness, *schedules) {
	t.Helper()
	r := rule("lights-on", homev1alpha1.RuleSpec{
		Triggers:  triggers,
		Condition: `trigger.type == "at" || trigger.type == "sun"`,
		Do:        []homev1alpha1.Action{set("hallway-light", capability.LightOn, "true")},
	})
	r.CreationTimestamp = metav1.NewTime(now.Add(-30 * 24 * time.Hour))
	if edit != nil {
		edit(r)
	}
	s := &schedules{}
	h := newHarnessWith(t, now, func(e *Engine) {
		e.Site, e.OnSchedule, e.NextChange = site, s.record, NewNextChangeGauge()
	}, r)
	return h, s
}

func (h *harness) result(name, result string) float64 {
	return testutil.ToFloat64(h.e.Runs.WithLabelValues(ns, name, result))
}

var lightOn = call{"hallway-light", capability.LightOn, true}

// advance sets the clock to t until the engine has made n calls. Setting it again fires a timer
// the engine created just after the first set.
func (h *harness) advance(t time.Time, n int) {
	h.t.Helper()
	h.eventually("the timer", h.clock.HasWaiters)
	h.eventually(fmt.Sprintf("%d calls at %v", n, t), func() bool {
		h.clock.SetTime(t)
		return len(h.setter.got()) >= n
	})
}

func TestAtFiresWithTheClock(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 06:59 BST"), nil, func(r *homev1alpha1.Rule) {
		r.Status.LastFire = metav1.NewTime(local("2026-10-20 07:00 BST"))
	}, atTrigger("07:00"))
	if got := s.last().Next; !got.Equal(local("2026-10-21 07:00 BST")) {
		t.Fatalf("next = %v, want 07:00", got)
	}
	h.advance(local("2026-10-21 07:00 BST"), 1)
	h.expectCalls(lightOn)
	h.eventually("the next fire", func() bool { return s.last().Next.Equal(local("2026-10-22 07:00 BST")) })
	if got := s.last().LastFire; !got.Equal(local("2026-10-21 07:00 BST")) {
		t.Errorf("lastFire = %v, want 07:00", got)
	}
	if got := testutil.ToFloat64(h.e.NextChange.WithLabelValues(ns, "lights-on")); got != float64(local("2026-10-22 07:00 BST").Unix()) {
		t.Errorf("next-change gauge = %v", got)
	}
}

func TestSunFiresWithTheClock(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 12:00 BST"), londonSite, nil,
		sunsetTrigger(-15*time.Minute))
	set, _ := londonSite.Time(sun.Sunset, 2026, time.October, 21, london)
	want := set.Add(-15 * time.Minute)
	if got := s.last().Next; !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	h.advance(want, 1)
	h.expectCalls(lightOn)
}

func TestMissedFireInsideTheDeadlineRunsOnStartup(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 07:05 BST"), nil, func(r *homev1alpha1.Rule) {
		r.Spec.StartingDeadline = &metav1.Duration{Duration: 10 * time.Minute}
		r.Status.LastFire = metav1.NewTime(local("2026-10-20 07:00 BST"))
	}, atTrigger("07:00"))
	h.expectCalls(lightOn)
	h.eventually("lastFire", func() bool { return s.last().LastFire.Equal(local("2026-10-21 07:00 BST")) })
}

func TestMissedFireOutsideTheDeadlineIsCounted(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 07:20 BST"), nil, func(r *homev1alpha1.Rule) {
		r.Spec.StartingDeadline = &metav1.Duration{Duration: 10 * time.Minute}
		r.Status.LastFire = metav1.NewTime(local("2026-10-20 07:00 BST"))
	}, atTrigger("07:00"))
	if got := h.result("lights-on", ResultMissed); got != 1 {
		t.Errorf("missed = %v, want 1", got)
	}
	if got := s.last().LastFire; !got.Equal(local("2026-10-21 07:00 BST")) {
		t.Errorf("lastFire = %v, want 07:00, so the next restart doesn't count it again", got)
	}
	time.Sleep(50 * time.Millisecond)
	if calls := h.setter.got(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
}

func TestMissedFireWithoutADeadlineIsSkipped(t *testing.T) {
	h, _ := timedHarness(t, local("2026-10-21 07:01 BST"), nil, func(r *homev1alpha1.Rule) {
		r.Status.LastFire = metav1.NewTime(local("2026-10-20 07:00 BST"))
	}, atTrigger("07:00"))
	if got := h.result("lights-on", ResultMissed); got != 1 {
		t.Errorf("missed = %v, want 1", got)
	}
}

func TestOnlyFiresSinceTheRuleWasCreated(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 07:05 BST"), nil, func(r *homev1alpha1.Rule) {
		r.Spec.StartingDeadline = &metav1.Duration{Duration: time.Hour}
		r.CreationTimestamp = metav1.NewTime(local("2026-10-21 07:02 BST"))
	}, atTrigger("07:00"))
	time.Sleep(50 * time.Millisecond)
	if calls := h.setter.got(); len(calls) != 0 || h.result("lights-on", ResultMissed) != 0 {
		t.Errorf("calls = %v; a Rule created after 07:00 shouldn't fire for it", calls)
	}
	if !s.last().LastFire.IsZero() {
		t.Errorf("lastFire = %v, want unchanged", s.last().LastFire)
	}
}

func TestReloadDoesNotCatchUp(t *testing.T) {
	h, _ := timedHarness(t, local("2026-10-21 07:20 BST"), nil, func(r *homev1alpha1.Rule) {
		r.Status.LastFire = metav1.NewTime(local("2026-10-20 07:00 BST"))
	}, atTrigger("07:00"))
	r := rule("lights-on", homev1alpha1.RuleSpec{
		Triggers: []homev1alpha1.Trigger{atTrigger("07:00")}, StartingDeadline: &metav1.Duration{Duration: time.Hour},
		Do: []homev1alpha1.Action{set("hallway-light", capability.LightOn, "true")},
	})
	r.Generation = 2
	r.Status.LastFire = metav1.NewTime(local("2026-10-20 07:00 BST"))
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	h.e.Load(c)
	time.Sleep(50 * time.Millisecond)
	if calls := h.setter.got(); len(calls) != 0 {
		t.Errorf("calls = %v; only the first load looks for missed fires", calls)
	}
}

func TestSunWithoutASiteIsReported(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 12:00 BST"), nil, nil,
		sunsetTrigger(0), atTrigger("23:00"))
	sched := s.last()
	if sched.Reason != ReasonNoLocation {
		t.Errorf("reason = %q, want %q", sched.Reason, ReasonNoLocation)
	}
	// The at trigger still fires.
	if !sched.Next.Equal(local("2026-10-21 23:00 BST")) {
		t.Errorf("next = %v, want 23:00", sched.Next)
	}
	h.advance(local("2026-10-21 23:00 BST"), 1)
	h.expectCalls(lightOn)
}

func TestEditingAwayTimersClearsTheSchedule(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 06:00 BST"), nil, nil, atTrigger("07:00"))
	r := rule("lights-on", homev1alpha1.RuleSpec{Triggers: motionTrigger, Do: []homev1alpha1.Action{delay(time.Second)}})
	r.Generation = 2
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	h.e.Load(c)
	if got := s.last(); !got.Cleared || got.Generation != 2 {
		t.Errorf("got %+v, want the schedule cleared for generation 2", got)
	}
	if n := testutil.CollectAndCount(h.e.NextChange); n != 0 {
		t.Errorf("next-change gauge has %d series, want none", n)
	}
}

func TestRemoveClearsTheScheduleAndForgetsTheLoad(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 06:00 BST"), nil, nil, atTrigger("07:00"))
	h.e.Remove(types.NamespacedName{Namespace: ns, Name: "lights-on"})
	if !s.last().Cleared {
		t.Errorf("got %+v, want the schedule cleared", s.last())
	}
	// Recreated, it looks for missed fires again, but only since it was created.
	h.clock.SetTime(local("2026-10-21 07:05 BST"))
	r := rule("lights-on", homev1alpha1.RuleSpec{
		Triggers: []homev1alpha1.Trigger{atTrigger("07:00")}, StartingDeadline: &metav1.Duration{Duration: time.Hour},
		Do: []homev1alpha1.Action{set("hallway-light", capability.LightOn, "true")},
	})
	r.CreationTimestamp = metav1.NewTime(local("2026-10-21 07:01 BST"))
	// The old Rule's lastFire is older than the new one.
	r.Status.LastFire = metav1.NewTime(local("2026-10-20 07:00 BST"))
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	h.e.Load(c)
	time.Sleep(50 * time.Millisecond)
	if calls := h.setter.got(); len(calls) != 0 {
		t.Errorf("calls = %v; 07:00 was before the new Rule existed", calls)
	}
}

func TestPolarNightLooksAgain(t *testing.T) {
	tromso := &sun.Site{Latitude: 69.6492, Longitude: 18.9553}
	h, s := timedHarness(t, local("2026-12-10 12:00 GMT"), tromso, nil,
		homev1alpha1.Trigger{Sun: &homev1alpha1.SunTrigger{Event: homev1alpha1.SunEventSunrise}})
	if got := s.last().Next; !got.IsZero() {
		t.Fatalf("next = %v, want none in the polar night", got)
	}
	h.eventually("the recheck timer", h.clock.HasWaiters)
	h.eventually("a sunrise in late January", func() bool {
		h.clock.SetTime(local("2027-01-25 00:00 GMT"))
		return !s.last().Next.IsZero()
	})
}

func TestCompileRejectsSignedTimes(t *testing.T) {
	for _, tm := range []string{"-1:30", "+7:05", "07:5x"} {
		if _, err := compileAt(&homev1alpha1.AtTrigger{Time: tm}); err == nil {
			t.Errorf("%q compiled", tm)
		}
	}
}
