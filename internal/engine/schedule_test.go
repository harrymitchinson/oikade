package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/ident"
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

func atTrigger(hhmm string, days ...time.Weekday) Timer { return At{Time: hhmm, Days: days} }

func sunsetTrigger(offset time.Duration) Timer { return SunTime{Event: sun.Sunset, Offset: offset} }

func compiled(t *testing.T, triggers ...Timer) *Compiled {
	t.Helper()
	c := momentRule(t, "timed", Moment{Timers: triggers, Do: []Action{delay(time.Second)}})
	return c
}

// follow returns the next n fires after t.
func follow(c *Compiled, t time.Time, n int, site *sun.Site) []time.Time {
	var out []time.Time
	suns := newSunCache(site)
	for range n {
		f, ok := c.nextFire(t, london, suns)
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
	got := follow(compiled(t, atTrigger("00:30", time.Saturday, time.Sunday)), local("2026-10-23 12:00 BST"), 3, nil)
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
		trigger Timer
		wantErr string
	}{
		"short time":    {atTrigger("7:00"), "is not HH:MM"},
		"bad time":      {atTrigger("24:00"), "is not HH:MM"},
		"bad day":       {atTrigger("07:00", 7), "unknown day"},
		"long offset":   {sunsetTrigger(7 * time.Hour), "between -6h and 6h"},
		"unknown event": {SunTime{Event: 9}, "unknown event"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewMoment(goKey("timed"), Moment{Timers: []Timer{tc.trigger}, Do: []Action{delay(time.Second)}})
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

func (s *schedules) record(_ ident.ID, sched Schedule) {
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
func timedHarness(t *testing.T, now time.Time, site *sun.Site, edit func(*Moment), triggers ...Timer) (*harness, *schedules) {
	t.Helper()
	m := Moment{Timers: triggers, Condition: timerFired, Since: now.Add(-30 * 24 * time.Hour), Do: []Action{set("hallway-light", capability.On, "true")}}
	if edit != nil {
		edit(&m)
	}
	r := momentRule(t, "lights-on", m)
	s := &schedules{}
	h := newHarnessWith(t, now, func(e *Engine) {
		e.Site, e.suns, e.OnSchedule, e.NextChange = site, newSunCache(site), s.record, NewNextChangeGauge()
	}, r)
	return h, s
}

func (h *harness) result(name, result string) float64 {
	return testutil.ToFloat64(h.e.Runs.WithLabelValues(name, result))
}

var lightOn = call{"hallway-light", capability.On, true}

// advance sets the clock to t once the engine has caught up, and waits for it to make n calls.
func (h *harness) advance(t time.Time, n int) {
	h.t.Helper()
	h.settle()
	h.clock.SetTime(t)
	h.settle()
	if got := h.setter.got(); len(got) < n {
		h.t.Fatalf("%d calls at %v, want %d: %v", len(got), t, n, got)
	}
}

func TestAtFiresWithTheClock(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 06:59 BST"), nil, func(r *Moment) {
		r.Since = local("2026-10-20 07:00 BST")
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
	if got := testutil.ToFloat64(h.e.NextChange.WithLabelValues("lights-on")); got != float64(local("2026-10-22 07:00 BST").Unix()) {
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
	h, s := timedHarness(t, local("2026-10-21 07:05 BST"), nil, func(r *Moment) {
		r.StartingDeadline = 10 * time.Minute
		r.Since = local("2026-10-20 07:00 BST")
	}, atTrigger("07:00"))
	h.expectCalls(lightOn)
	h.eventually("lastFire", func() bool { return s.last().LastFire.Equal(local("2026-10-21 07:00 BST")) })
}

func TestMissedFireOutsideTheDeadlineIsCounted(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 07:20 BST"), nil, func(r *Moment) {
		r.StartingDeadline = 10 * time.Minute
		r.Since = local("2026-10-20 07:00 BST")
	}, atTrigger("07:00"))
	if got := h.result("lights-on", ResultMissed); got != 1 {
		t.Errorf("missed = %v, want 1", got)
	}
	if got := s.last().LastFire; !got.Equal(local("2026-10-21 07:00 BST")) {
		t.Errorf("lastFire = %v, want 07:00, so the next restart doesn't count it again", got)
	}
	h.settle()
	if calls := h.setter.got(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
}

func TestMissedFireWithoutADeadlineIsSkipped(t *testing.T) {
	h, _ := timedHarness(t, local("2026-10-21 07:01 BST"), nil, func(r *Moment) {
		r.Since = local("2026-10-20 07:00 BST")
	}, atTrigger("07:00"))
	if got := h.result("lights-on", ResultMissed); got != 1 {
		t.Errorf("missed = %v, want 1", got)
	}
}

func TestOnlyFiresSinceTheRuleWasCreated(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 07:05 BST"), nil, func(r *Moment) {
		r.StartingDeadline = time.Hour
		r.Since = local("2026-10-21 07:02 BST")
	}, atTrigger("07:00"))
	h.settle()
	if calls := h.setter.got(); len(calls) != 0 || h.result("lights-on", ResultMissed) != 0 {
		t.Errorf("calls = %v; a Rule created after 07:00 shouldn't fire for it", calls)
	}
	if !s.last().LastFire.IsZero() {
		t.Errorf("lastFire = %v, want unchanged", s.last().LastFire)
	}
}

func TestReloadDoesNotCatchUp(t *testing.T) {
	h, _ := timedHarness(t, local("2026-10-21 07:20 BST"), nil, func(r *Moment) {
		r.Since = local("2026-10-20 07:00 BST")
	}, atTrigger("07:00"))
	r := momentRule(t, "lights-on", Moment{Timers: []Timer{At{Time: "07:00"}}, StartingDeadline: time.Hour, Since: local("2026-10-20 07:00 BST"), Generation: 2, Do: []Action{set("hallway-light", capability.On, "true")}})
	h.e.Load(r)
	h.settle()
	if calls := h.setter.got(); len(calls) != 0 {
		t.Errorf("calls = %v; only the first load looks for missed fires", calls)
	}
}

// An engine with no Site, as for arbitration alone, never fires a sun time. A home with one fails
// oikade check, so only a test gets here.
func TestSunWithoutASiteNeverFires(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 12:00 BST"), nil, nil,
		sunsetTrigger(0), atTrigger("23:00"))
	sched := s.last()
	// The at trigger still fires.
	if !sched.Next.Equal(local("2026-10-21 23:00 BST")) {
		t.Errorf("next = %v, want 23:00", sched.Next)
	}
	h.advance(local("2026-10-21 23:00 BST"), 1)
	h.expectCalls(lightOn)
}

func TestEditingAwayTimersClearsTheSchedule(t *testing.T) {
	h, s := timedHarness(t, local("2026-10-21 06:00 BST"), nil, nil, atTrigger("07:00"))
	r := momentRule(t, "lights-on", Moment{Changes: motionTrigger, Do: []Action{delay(time.Second)}})
	r.Generation = 2
	c := r
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
	h.e.Remove(ident.ID{Name: "lights-on"})
	if !s.last().Cleared {
		t.Errorf("got %+v, want the schedule cleared", s.last())
	}
	// Recreated, it looks for missed fires again, but only since it was created.
	h.clock.SetTime(local("2026-10-21 07:05 BST"))
	h.e.Load(momentRule(t, "lights-on", Moment{Timers: []Timer{At{Time: "07:00"}}, StartingDeadline: time.Hour, Since: local("2026-10-21 07:01 BST"), Do: []Action{set("hallway-light", capability.On, "true")}}))
	h.settle()
	if calls := h.setter.got(); len(calls) != 0 {
		t.Errorf("calls = %v; 07:00 was before the new rule existed", calls)
	}
}

func TestPolarNightLooksAgain(t *testing.T) {
	tromso := &sun.Site{Latitude: 69.6492, Longitude: 18.9553}
	h, s := timedHarness(t, local("2026-12-10 12:00 GMT"), tromso, nil,
		SunTime{Event: sun.Sunrise})
	if got := s.last().Next; !got.IsZero() {
		t.Fatalf("next = %v, want none in the polar night", got)
	}
	h.settle()
	h.clock.SetTime(local("2027-01-25 00:00 GMT"))
	h.settle()
	if s.last().Next.IsZero() {
		t.Error("no sunrise found in late January")
	}
}

func TestCompileRejectsSignedTimes(t *testing.T) {
	for _, tm := range []string{"-1:30", "+7:05", "07:5x"} {
		if _, err := atTime(tm); err == nil {
			t.Errorf("%q compiled", tm)
		}
	}
}

func TestSunCacheAnswersAsTheSiteDoes(t *testing.T) {
	tromso := sun.Site{Latitude: 69.6492, Longitude: 18.9553}
	for _, month := range []time.Month{time.March, time.June, time.December} { // ordinary days, then none
		suns := newSunCache(&tromso)
		for pass := range 2 {
			for day := range 20 {
				for _, event := range []sun.Event{sun.Sunrise, sun.Sunset} {
					want, wantOK := tromso.Time(event, 2026, month, 1+day, london)
					got, ok := suns.time(event, 2026, month, 1+day, london)
					if ok != wantOK || !got.Equal(want) {
						t.Fatalf("pass %d: %v on %v %d = %v, %t, want %v, %t", pass, event, month, 1+day, got, ok, want, wantOK)
					}
				}
			}
		}
	}
}

func TestSunCacheRemembersADayWithNoSunset(t *testing.T) {
	suns := newSunCache(londonSite)
	suns.days = map[sunDay]sunAnswer{{event: sun.Sunset, year: 2026, month: time.October, day: 21, loc: london}: {}}
	if at, ok := suns.time(sun.Sunset, 2026, time.October, 21, london); ok || !at.IsZero() {
		t.Errorf("got %v, %t, want the cached day with no sunset", at, ok)
	}
}

func TestSunCacheIsWhatAWindowReads(t *testing.T) {
	c := holdRule(t, "porch", Hold{Device: "d", Capability: capability.On, Value: true, Window: &Window{Start: SunTime{Event: sun.Sunset}, End: At{Time: "23:00"}}})
	suns := newSunCache(londonSite)
	seeded := local("2026-10-21 17:00 BST")
	suns.days = map[sunDay]sunAnswer{{event: sun.Sunset, year: 2026, month: time.October, day: 21, loc: london}: {at: seeded, ok: true}}
	if _, next := c.hold.window.at(local("2026-10-21 12:00 BST"), london, suns); !next.Equal(seeded) {
		t.Errorf("window starts %v, want the cached %v", next, seeded)
	}
}

func TestSunCacheStaysBounded(t *testing.T) {
	suns := newSunCache(londonSite)
	for day := range 1000 {
		d := time.Date(2026, time.January, 1+day, 12, 0, 0, 0, london)
		suns.time(sun.Sunset, d.Year(), d.Month(), d.Day(), london)
		if len(suns.days) > sunCacheSize {
			t.Fatalf("cache holds %d days after %d, want at most %d", len(suns.days), day, sunCacheSize)
		}
	}
}

func TestSunCacheWithoutASite(t *testing.T) {
	if _, ok := newSunCache(nil).time(sun.Sunset, 2026, time.October, 21, london); ok {
		t.Error("no site gave a sunset")
	}
}
