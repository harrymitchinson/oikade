package capability

import (
	"encoding/json"
	"math"
	"testing"
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
