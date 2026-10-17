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
		{LightOn, true, true, true},
		{LightOn, "ON", nil, false},
		{LightOn, 1.0, nil, false},
		{LightLevel, 40, 40.0, true},
		{LightLevel, json.Number("12.5"), 12.5, true},
		{LightLevel, 101.0, nil, false},
		{LightLevel, -1.0, nil, false},
		{LightLevel, math.NaN(), nil, false},
		{LightLevel, true, nil, false},
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
	if v, err := ParseJSON(LightLevel, []byte("40")); err != nil || v != 40.0 {
		t.Errorf("ParseJSON(level, 40) = %v, %v", v, err)
	}
	if _, err := ParseJSON(LightOn, []byte(`"true"`)); err == nil {
		t.Error("ParseJSON accepted a string for a bool capability")
	}
}

func TestFloat(t *testing.T) {
	if Float(true) != 1 || Float(false) != 0 || Float(21.5) != 21.5 {
		t.Error("Float conversion is wrong")
	}
}

func TestRegistryStatus(t *testing.T) {
	for name, s := range registry {
		st, ok := Status(name)
		if !ok || st.Name != name || st.Kind != s.Kind || st.Writable != s.Writable {
			t.Errorf("Status(%s) = %+v", name, st)
		}
	}
}
