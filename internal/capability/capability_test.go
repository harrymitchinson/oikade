package capability

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		want any
		ok   bool
	}{
		{On, true, true, true},
		{On, "ON", nil, false},
		{On, 1.0, nil, false},
		{Brightness, 40, 40.0, true},
		{Brightness, json.Number("12.5"), 12.5, true},
		{Brightness, 101.0, nil, false},
		{Brightness, -1.0, nil, false},
		{Brightness, math.NaN(), nil, false},
		{Brightness, true, nil, false},
		{Temperature, -12.3, -12.3, true},
		{LinkQuality, 255, 255.0, true},
		{Mode, "dry", "dry", true},
		{Mode, "off", nil, false},
		{Mode, "Dry", nil, false},
		{Mode, 3.0, nil, false},
		{Mode, true, nil, false},
		{"made.up", true, nil, false},
	} {
		got, err := Validate(tc.name, tc.v)
		if (err == nil) != tc.ok {
			t.Errorf("Validate(%s, %v) error = %v, want ok=%v", tc.name, tc.v, err, tc.ok)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("Validate(%s, %v) = %v (%T), want %v", tc.name, tc.v, got, got, tc.want)
		}
	}
}

func TestParseJSON(t *testing.T) {
	if v, err := ParseJSON(Brightness, []byte("40")); err != nil || v != 40.0 {
		t.Errorf("ParseJSON(level, 40) = %v, %v", v, err)
	}
	if _, err := ParseJSON(On, []byte(`"true"`)); err == nil {
		t.Error("ParseJSON accepted a string for a bool capability")
	}
}

func TestFloat(t *testing.T) {
	if Float(true) != 1 || Float(false) != 0 || Float(21.5) != 21.5 {
		t.Error("Float conversion is wrong")
	}
}

// A mode's number is fixed: claims hold it and PromQL reads it, so a name never changes number.
func TestModeNumbersAreFixed(t *testing.T) {
	for want, name := range []string{"auto", "heat", "cool", "dry", "fan"} {
		if got := Float(name); got != float64(want) {
			t.Errorf("Float(%q) = %v, want %d", name, got, want)
		}
		if got, ok := NameOf(Mode, float64(want)); !ok || got != name {
			t.Errorf("NameOf(mode, %d) = %q, %v, want %q", want, got, ok, name)
		}
	}
	for _, f := range []float64{-1, 0.5, 5, math.NaN()} {
		if got, ok := NameOf(Mode, f); ok {
			t.Errorf("NameOf(mode, %v) = %q, want none", f, got)
		}
	}
	if _, ok := NameOf(On, 1); ok {
		t.Error("NameOf named a bool's number")
	}
}

// Float turns a name into its number without the capability, so a name has one number everywhere.
func TestANameHasOneNumberInEveryCapability(t *testing.T) {
	seen := map[string]float64{}
	for _, c := range Names() {
		s := registry[c]
		if s.Kind != KindName {
			if len(s.Names) > 0 {
				t.Errorf("%s isn't a KindName and has names", c)
			}
			continue
		}
		if len(s.Names) == 0 {
			t.Errorf("%s has no names", c)
		}
		for i, n := range s.Names {
			if f, ok := seen[n]; ok && f != float64(i) {
				t.Errorf("%s is %v in one capability and %d in %s", n, f, i, c)
			}
			seen[n] = float64(i)
		}
	}
}

// A driver's rounding is the value written; a step of 1, or a bool's other value, isn't.
func TestNear(t *testing.T) {
	for _, c := range []struct {
		reported, wrote float64
		want            bool
	}{
		{33.1, 33, true},
		{40.2, 40, true},
		{39.5, 40, true},
		{79, 80, false},
		{0, 1, false},
	} {
		if got := Near(c.reported, c.wrote); got != c.want {
			t.Errorf("Near(%v, %v) = %v, want %v", c.reported, c.wrote, got, c.want)
		}
	}
}

// Every capability but those that travel or turn keeps the default settle.
func TestOnlyPositionAndLockedSettleForLonger(t *testing.T) {
	for _, name := range Names() {
		want := DefaultSettle
		switch name {
		case Position:
			want = 2 * time.Minute
		case Locked:
			want = 30 * time.Second
		}
		if got := SettleOf(name); got != want {
			t.Errorf("%s settles for %v, want %v", name, got, want)
		}
	}
	if got := SettleOf("no-such-capability"); got != DefaultSettle {
		t.Errorf("an unknown capability settles for %v", got)
	}
}
