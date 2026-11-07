package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/control"
)

func controlEntry(capability, def, resolve string) control.Entry {
	return control.Entry{Capability: capability, Default: []byte(def), Resolve: resolve, External: control.NameRevert}
}

func TestControlSettingsBool(t *testing.T) {
	c := controlEntry("on", "true", control.NamePreferOff)
	c.External, c.OverrideHold = control.NameOverride, "2h"
	c.MinimumOn, c.MaximumOn = control.Duration(time.Hour), control.Duration(6*time.Hour)
	c.MinimumOff, c.MaximumStartsPerDay = control.Duration(30*time.Minute), 4

	got, err := ControlSettings(c, london)
	if err != nil {
		t.Fatal(err)
	}
	want := control.Settings{
		Kind: control.Bool, Default: 1, Resolve: control.Off, External: control.ExternalOverride,
		OverrideHold: 2 * time.Hour, MinimumOn: time.Hour, MaximumOn: 6 * time.Hour, MinimumOff: 30 * time.Minute,
		MaximumStartsPerDay: 4, Location: london,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestControlSettingsNumberUntilNext(t *testing.T) {
	c := controlEntry("brightness", "40", control.NameHighest)
	c.External, c.OverrideHold = control.NameOverride, control.NameNext

	got, err := ControlSettings(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != control.Number || got.Default != 40 || got.Resolve != control.Highest || !got.OverrideUntilNext {
		t.Errorf("got %+v", got)
	}
}

func TestControlSettingsRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		entry control.Entry
		want  string
	}{
		"unknown capability": {controlEntry("light.hue", "1", control.NameHighest), "unknown capability"},
		"read-only":          {controlEntry("temperature", "20", control.NameHighest), "read-only"},
		"number resolve on a bool": {
			controlEntry("on", "false", control.NameHighest), "resolve must be PreferOn or PreferOff",
		},
		"bool resolve on a number": {
			controlEntry("brightness", "0", control.NamePreferOn), "resolve must be Highest or Lowest",
		},
		"default of the wrong kind": {controlEntry("on", "1", control.NamePreferOn), "takes a bool"},
		"default out of range":      {controlEntry("brightness", "150", control.NameLowest), "between 0 and 100"},
		"override without a hold": {func() control.Entry {
			c := controlEntry("on", "false", control.NamePreferOn)
			c.External = control.NameOverride
			return c
		}(), "needs an override hold"},
		"maximumOn shorter than minimumOn": {func() control.Entry {
			c := controlEntry("on", "false", control.NamePreferOn)
			c.MinimumOn, c.MaximumOn = control.Duration(time.Hour), control.Duration(time.Minute)
			return c
		}(), "maximumOn must not be shorter than minimumOn"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ControlSettings(tc.entry, nil)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "control "+tc.entry.Capability+": ") {
				t.Errorf("got %q, want it to name the capability and contain %q", err, tc.want)
			}
		})
	}
}
