package ewelinklan

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

// The emulator's key in sonoff-lan, the TypeScript library this was ported from.
const fixtureKey = "fixture-lan-key-not-real"

// Vectors made with Node's crypto as sonoff-lan calls it, so these check the port against the
// original rather than against itself.
const fixtureIV = "AAECAwQFBgcICQoLDA0ODw=="

var vectors = []struct {
	name, data string
	want       map[string]any
}{
	{
		"th_plug", "Ep/hznfOHN04d/IhmEJFDKLOC5tBTwZdY6G5Voxd0ngpqAYsKLGiea7FJSJnf+7r",
		map[string]any{"currentTemperature": "19.2", "switch": "on"},
	},
	{
		"plug", "s+ghx1L9Wr+CaTslF7hAPa6ErNt1N+k6pftk7u+wfB8Il1EOwYBl2JTOLYMkO2Wy",
		map[string]any{"switches": []any{map[string]any{"outlet": float64(0), "switch": "off"}}},
	},
}

func TestDecryptsWhatTheOriginalEncrypts(t *testing.T) {
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			// Split as a device splits it, over the TXT record's fields.
			iv, data, err := ciphertext(map[string]string{"iv": fixtureIV, "data1": v.data[:40], "data2": v.data[40:]})
			if err != nil {
				t.Fatal(err)
			}
			got, err := decrypt(iv, data, lanKey(fixtureKey))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, v.want) {
				t.Errorf("got %v, want %v", got, v.want)
			}
		})
	}
}

func TestAWrongKeyIsNotMalformed(t *testing.T) {
	iv, data, err := ciphertext(map[string]string{"iv": fixtureIV, "data1": vectors[0].data})
	if err != nil {
		t.Fatal(err)
	}
	_, err = decrypt(iv, data, lanKey("another-key"))
	if err == nil || errors.Is(err, errMalformed) {
		t.Errorf("got %v", err)
	}
}

func TestMalformedReports(t *testing.T) {
	good := vectors[0].data
	for name, txt := range map[string]map[string]string{
		"no iv":        {"data1": good},
		"short iv":     {"iv": "AAEC", "data1": good},
		"iv not b64":   {"iv": "!!!", "data1": good},
		"no data":      {"iv": fixtureIV},
		"data not b64": {"iv": fixtureIV, "data1": "!!!"},
		"not blocks":   {"iv": fixtureIV, "data1": base64.StdEncoding.EncodeToString([]byte("short"))},
		"a gap":        {"iv": fixtureIV, "data2": good},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ciphertext(txt); !errors.Is(err, errMalformed) {
				t.Errorf("got %v", err)
			}
		})
	}
}

func TestEncryptIsWhatDecryptReads(t *testing.T) {
	key := lanKey(fixtureKey)
	iv, data, err := encrypt(kinds["th_plug"].command(true), key)
	if err != nil {
		t.Fatal(err)
	}
	rawIV, rawData, err := ciphertext(map[string]string{"iv": iv, "data1": data})
	if err != nil {
		t.Fatal(err)
	}
	got, err := decrypt(rawIV, rawData, key)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"deviceType": "normal", "mainSwitch": "on", "switch": "on"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestReadingsAreNumbersWithinRange(t *testing.T) {
	for _, tt := range []struct {
		value any
		want  float64
		ok    bool
	}{
		{"19.2", 19.2, true},
		{"+4", 4, true},
		{".5", 0.5, true},
		{-3.5, -3.5, true},
		{"unavailable", 0, false},
		{"1e3", 0, false},
		{"NaN", 0, false},
		{"Inf", 0, false},
		{"", 0, false},
		{"101", 0, false},
		{"-271", 0, false},
		{true, 0, false},
		{nil, 0, false},
	} {
		got, ok := reading(tt.value, -270, 100)
		if ok != tt.ok || ok && got != tt.want {
			t.Errorf("reading(%v) = %v, %v", tt.value, got, ok)
		}
	}
}

func TestSwitchesReadsOnlyOutletZero(t *testing.T) {
	outlet := func(n float64, state any) any { return map[string]any{"outlet": n, "switch": state} }
	for name, tt := range map[string]struct {
		switches any
		want     report
	}{
		"on":            {[]any{outlet(0, "on")}, report{relay: true, on: true}},
		"off":           {[]any{outlet(0, "off")}, report{relay: true}},
		"other outlets": {[]any{outlet(1, "on"), outlet(0, "off")}, report{relay: true}},
		"only others":   {[]any{outlet(1, "on")}, report{}},
		"conflicting":   {[]any{outlet(0, "on"), outlet(0, "off")}, report{relay: true, bad: true}},
		"not on or off": {[]any{outlet(0, "maybe")}, report{relay: true, bad: true}},
		"not a list":    {"on", report{}},
		"entry isn't":   {[]any{"on"}, report{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := readSwitches(map[string]any{"switches": tt.switches}); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v", got)
			}
		})
	}
}

// A THR320D's report with a WTS01 probe, which measures only temperature, decrypted on 2026-11-12.
// Its network fields are left out.
const realTHReport = `{"autoControlEnabled":0,"currentHumidity":"unavailable","currentTemperature":"11.9",` +
	`"fwVersion":"1.3.0","humCorrection":0,"pulseConfig":{"pulse":"off","pulseWidth":500,"switch":"off"},` +
	`"rssi":-62,"sensorType":"WTS01","sledOnline":"on","startup":"off","switch":"off","tempCorrection":0,` +
	`"tempUnit":0,"timeZone":1}`

func TestReadsARealTHReport(t *testing.T) {
	var params map[string]any
	if err := json.Unmarshal([]byte(realTHReport), &params); err != nil {
		t.Fatal(err)
	}
	got := kinds["th_plug"].read(params)
	want := report{relay: true, on: false, values: map[string]float64{contractv1.Temperature: 11.9}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
