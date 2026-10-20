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

package sun

import (
	"testing"
	"time"
)

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// The expected times are the US Naval Observatory's (aa.usno.navy.mil/api/rstt/oneday), which
// round to the minute.
func TestMatchesPublishedTables(t *testing.T) {
	london := Site{51.5074, -0.1278}
	sydney := Site{-33.8688, 151.2093}
	for _, tc := range []struct {
		name     string
		site     Site
		zone     string
		date     string
		rise     string
		set      string
		nextDays int // the sunset is this many days after date
	}{
		{"London, the day before BST", london, "Europe/London", "2026-03-28", "05:45", "18:27", 0},
		{"London, the first day of BST", london, "Europe/London", "2026-03-29", "06:43", "19:29", 0},
		{"London, midwinter", london, "Europe/London", "2026-12-21", "08:04", "15:53", 0},
		{"Sydney, the day before AEDT", sydney, "Australia/Sydney", "2026-10-03", "05:30", "17:59", 0},
		{"Sydney, the first day of AEDT", sydney, "Australia/Sydney", "2026-10-04", "06:29", "19:00", 0},
		{"New York, winter", Site{40.7128, -74.0060}, "America/New_York", "2026-01-15", "07:18", "16:53", 0},
		{"Reykjavik, midsummer, sunset after midnight", Site{64.1466, -21.9426}, "Atlantic/Reykjavik", "2026-06-21", "02:55", "00:04", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := mustLocation(tc.zone)
			d, err := time.ParseInLocation(time.DateOnly, tc.date, loc)
			if err != nil {
				t.Fatal(err)
			}
			check := func(e Event, want string, days int) {
				t.Helper()
				got, ok := tc.site.Time(e, d.Year(), d.Month(), d.Day(), loc)
				if !ok {
					t.Fatalf("event %d: no time", e)
				}
				w, err := time.ParseInLocation("2006-01-02 15:04", d.AddDate(0, 0, days).Format(time.DateOnly)+" "+want, loc)
				if err != nil {
					t.Fatal(err)
				}
				if diff := got.Sub(w); diff < -time.Minute || diff > time.Minute {
					t.Errorf("event %d: got %v, want %v within a minute", e, got, w)
				}
			}
			check(Sunrise, tc.rise, 0)
			check(Sunset, tc.set, tc.nextDays)
		})
	}
}

func TestPolarDayHasNoSunset(t *testing.T) {
	tromso := Site{69.6492, 18.9553}
	loc := mustLocation("Europe/Oslo")
	for _, e := range []Event{Sunrise, Sunset} {
		if got, ok := tromso.Time(e, 2026, time.June, 21, loc); ok {
			t.Errorf("event %d: got %v, want none in the midnight sun", e, got)
		}
	}
	if _, ok := tromso.Time(Sunset, 2026, time.December, 21, loc); ok {
		t.Error("got a sunset in the polar night")
	}
}
