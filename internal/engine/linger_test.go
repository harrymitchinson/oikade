package engine

import (
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/sun"
)

func TestAnOffUntilEndsAnOffDelayAtTheNextTime(t *testing.T) {
	tromso := &sun.Site{Latitude: 69.6492, Longitude: 18.9553}
	set, _ := londonSite.Time(sun.Sunset, 2026, time.November, 23, london)
	for name, tc := range map[string]struct {
		delay time.Duration
		until Timer
		site  *sun.Site
		now   time.Time
		want  time.Time
	}{
		"a Linger alone": {
			delay: time.Hour, now: local("2026-11-23 08:00 GMT"), want: local("2026-11-23 09:00 GMT"),
		},
		"the next midnight": {
			until: atTrigger("00:00"), now: local("2026-11-23 15:00 GMT"), want: local("2026-11-24 00:00 GMT"),
		},
		"after a Linger, seen in the morning": {
			delay: 24 * time.Hour, until: atTrigger("00:00"),
			now: local("2026-11-23 08:00 GMT"), want: local("2026-11-25 00:00 GMT"),
		},
		"after a Linger, seen late": {
			delay: 24 * time.Hour, until: atTrigger("00:00"),
			now: local("2026-11-23 23:30 GMT"), want: local("2026-11-25 00:00 GMT"),
		},
		"a moment after the time, as a look at a window's end runs": {
			until: atTrigger("23:00"),
			now:   local("2026-11-23 23:00 GMT").Add(300 * time.Millisecond), want: local("2026-11-23 23:00 GMT"),
		},
		"more than a second after the time": {
			until: atTrigger("23:00"),
			now:   local("2026-11-23 23:00 GMT").Add(2 * time.Second), want: local("2026-11-24 23:00 GMT"),
		},
		"a time the clocks skip": {
			until: atTrigger("01:30"), now: local("2026-03-28 12:00 GMT"), want: local("2026-03-29 02:00 BST"),
		},
		"sunset": {
			until: sunsetTrigger(0), site: londonSite, now: local("2026-11-23 12:00 GMT"), want: set,
		},
		"no sunrise within a week": {
			delay: time.Hour, until: SunTime{Event: sun.Sunrise}, site: tromso,
			now: local("2026-12-10 12:00 GMT"), want: local("2026-12-10 13:00 GMT"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := &hold{offDelay: tc.delay}
			if tc.until != nil {
				until, err := offUntil(tc.until)
				if err != nil {
					t.Fatal(err)
				}
				h.offUntil = until
			}
			if got := h.offAt(tc.now, offClock{loc: london, suns: newSunCache(tc.site)}); !got.Equal(tc.want) {
				t.Errorf("offAt = %v, want %v", got.In(london), tc.want.In(london))
			}
		})
	}
}

func TestAnOffUntilKeepsAHoldActiveWithNoOffDelay(t *testing.T) {
	until, err := offUntil(atTrigger("23:00"))
	if err != nil {
		t.Fatal(err)
	}
	h := &hold{offUntil: until}
	st, active := h.step(HoldState{Active: true}, false, true, local("2026-11-23 21:05 GMT"), offClock{loc: london})
	if !active || !st.OffAt.Equal(local("2026-11-23 23:00 GMT")) {
		t.Fatalf("active = %t, offAt = %v, want active until 23:00", active, st.OffAt.In(london))
	}
	st, active = h.step(st, false, true, local("2026-11-23 23:00 GMT"), offClock{loc: london})
	if active || !st.OffAt.IsZero() {
		t.Fatalf("active = %t, offAt = %v at 23:00, want inactive", active, st.OffAt)
	}

	// A look a moment after the time, as a signal's on the minute, ends it then.
	st, active = h.step(HoldState{Active: true}, false, true, local("2026-11-23 23:00 GMT").Add(time.Millisecond),
		offClock{loc: london})
	if active {
		t.Fatalf("active = %t, offAt = %v, want it ended at once", active, st.OffAt.In(london))
	}
}

// A window that closes at the hold's offUntil ends it there, however late the look, as after a
// restart: its condition was false from the close.
func TestAnOffUntilCountsFromTheWindowsClose(t *testing.T) {
	until, err := offUntil(atTrigger("23:00"))
	if err != nil {
		t.Fatal(err)
	}
	h := &hold{offUntil: until}
	late := local("2026-11-23 23:05 GMT")
	st, active := h.step(HoldState{Active: true}, false, true, late,
		offClock{loc: london, closed: local("2026-11-23 23:00 GMT")})
	if active {
		t.Fatalf("active = %t, offAt = %v, want it ended at once", active, st.OffAt.In(london))
	}
	st, active = h.step(HoldState{Active: true}, false, true, late, offClock{loc: london})
	if !active || !st.OffAt.Equal(local("2026-11-24 23:00 GMT")) {
		t.Fatalf("active = %t, offAt = %v with no window, want tomorrow's 23:00", active, st.OffAt.In(london))
	}
}

func TestAWindowSaysWhenItClosed(t *testing.T) {
	w, err := (&Window{Start: atTrigger("20:00"), End: atTrigger("23:00")}).compile()
	if err != nil {
		t.Fatal(err)
	}
	for at, want := range map[string]time.Time{
		"2026-11-23 21:00 GMT": {},
		"2026-11-23 23:00 GMT": local("2026-11-23 23:00 GMT"),
		"2026-11-24 02:00 GMT": local("2026-11-23 23:00 GMT"),
		"2026-11-24 19:59 GMT": local("2026-11-23 23:00 GMT"),
		"2026-11-24 20:00 GMT": {},
	} {
		if got := w.closed(local(at), london, nil); !got.Equal(want) {
			t.Errorf("closed at %s = %v, want %v", at, got.In(london), want)
		}
	}
	night, err := (&Window{Start: atTrigger("22:00"), End: atTrigger("06:00"), Days: []time.Weekday{time.Friday}}).compile()
	if err != nil {
		t.Fatal(err)
	}
	// Friday night's window, Saturday 06:00, is the last to close by Monday noon.
	if got, want := night.closed(local("2026-11-23 12:00 GMT"), london, nil), local("2026-11-21 06:00 GMT"); !got.Equal(want) {
		t.Errorf("closed = %v, want %v", got.In(london), want)
	}
}

func TestNewHoldChecksItsOffUntil(t *testing.T) {
	when := isTrue("terrace-sensor", capability.Occupied)
	for name, tc := range map[string]struct {
		h    Hold
		want string
	}{
		"not a time": {
			Hold{Device: "terrace-light", Capability: capability.On, Value: true, When: when, OffUntil: atTrigger("25:00")},
			`hold.offUntil: time "25:00" is not HH:MM`,
		},
		"with days": {
			Hold{Device: "terrace-light", Capability: capability.On, Value: true, When: when, OffUntil: atTrigger("23:00", time.Friday)},
			"hold.offUntil: days belong on the window",
		},
		"on a follow": {
			Hold{
				Device: "terrace-light", Capability: capability.Brightness, OffUntil: atTrigger("23:00"),
				Follow: func(Vars) (float64, error) { return 1, nil },
			},
			"hold: follow must be the only condition",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewHold(ident.ID{Name: "terrace"}, tc.h)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}
