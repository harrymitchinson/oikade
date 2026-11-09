package v1

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// docBlocks returns the compacted ```json blocks of docs/plugin-contract.md, and the whole doc.
func docBlocks(t *testing.T) ([][]byte, string) {
	t.Helper()
	doc, err := os.ReadFile("../../docs/plugin-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile("(?s)```json\n(.*?)```").FindAllSubmatch(doc, -1)
	blocks := make([][]byte, 0, len(matches))
	for _, m := range matches {
		var b bytes.Buffer
		if err := json.Compact(&b, m[1]); err != nil {
			t.Fatalf("doc example is not JSON: %v\n%s", err, m[1])
		}
		blocks = append(blocks, b.Bytes())
	}
	return blocks, string(doc)
}

// roundTrip decodes want into a new T and checks it encodes back to the same bytes.
func roundTrip[T any](t *testing.T, want []byte) {
	t.Helper()
	var v T
	if err := json.Unmarshal(want, &v); err != nil {
		t.Fatalf("%T: %v", v, err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%T round trip:\n got %s\nwant %s", v, got, want)
	}
}

func TestPayloadsMatchContractDoc(t *testing.T) {
	blocks, doc := docBlocks(t)
	var devices, devicesFile int
	for _, b := range blocks {
		// The devices file is the only example with a contract field.
		if bytes.HasPrefix(b, []byte(`{"contract":`)) {
			roundTrip[DevicesFile](t, b)
			devicesFile++
		} else if bytes.HasPrefix(b, []byte(`{"devices":`)) {
			roundTrip[Devices](t, b)
			devices++
		}
	}
	if devices == 0 || devicesFile == 0 {
		t.Errorf("want devices and devices file examples in the doc, found %d and %d", devices, devicesFile)
	}

	for _, tc := range []struct {
		doc string
		got func() ([]byte, error)
	}{
		{`{"state":"online","contract":1}`, func() ([]byte, error) { return json.Marshal(Status{Online, Contract}) }},
		{`{"state":"offline","contract":1}`, func() ([]byte, error) { return json.Marshal(Status{Offline, Contract}) }},
		{`{"on":true}`, func() ([]byte, error) { return json.Marshal(State{"on": true}) }},
		{`{"on":false}`, SetCommand{ID: "1", Capability: "on", Value: false}.Payload},
	} {
		if !strings.Contains(doc, "`"+tc.doc+"`") {
			t.Errorf("the doc no longer shows %s", tc.doc)
		}
		got, err := tc.got()
		if err != nil || string(got) != tc.doc {
			t.Errorf("encoded %s, %v; want %s", got, err, tc.doc)
		}
	}
	for _, env := range []string{EnvMQTTURL, EnvTopic, EnvDevicesFile, EnvSecretsDir} {
		if !strings.Contains(doc, "`"+env+"`") {
			t.Errorf("the doc doesn't list %s", env)
		}
	}
}

func TestRoot(t *testing.T) {
	r := NewRoot("sonoff")
	for got, want := range map[string]string{
		string(r):                "oikade/v1/sonoff",
		r.Status():               "oikade/v1/sonoff/status",
		r.Devices():              "oikade/v1/sonoff/devices",
		r.State("1000aa"):        "oikade/v1/sonoff/1000aa/state",
		r.Availability("1000aa"): "oikade/v1/sonoff/1000aa/availability",
		r.Set("1000aa"):          "oikade/v1/sonoff/1000aa/set",
		r.SetFilter():            "oikade/v1/sonoff/+/set",
		r.Filter():               "oikade/v1/sonoff/#",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestParseRoot(t *testing.T) {
	if r, err := ParseRoot("oikade/v1/sonoff"); err != nil || r != NewRoot("sonoff") {
		t.Errorf("ParseRoot(valid) = %q, %v", r, err)
	}
	for _, bad := range []string{
		"",
		"oikade/v1",
		"oikade/v1/",
		"oikade/v1//sonoff",
		"oikade/v1/home/sonoff",
		"oikade/v1/_host",
		"oikade/v1/sonoff/",
		"oikade/v1/sonoff/extra",
		"oikade/v2/home/sonoff",
		"/oikade/v1/sonoff",
		"zigbee2mqtt/home/sonoff",
		"oikade/v1/+",
		"oikade/v1/#",
		"oikade/v1/so+noff",
		"oikade/v1/so\x00noff",
	} {
		if r, err := ParseRoot(bad); err == nil {
			t.Errorf("ParseRoot(%q) = %q, want an error", bad, r)
		}
	}
}

func TestParseSet(t *testing.T) {
	r := NewRoot("sonoff")
	for _, tc := range []struct {
		name    string
		topic   string
		payload string
		want    SetCommand
		ok      bool
		err     bool
	}{
		{"bool", "oikade/v1/sonoff/1000aa/set", `{"on":true}`,
			SetCommand{ID: "1000aa", Capability: "on", Value: true}, true, false},
		{"number", "oikade/v1/sonoff/1000aa/set", `{"brightness":42.5}`,
			SetCommand{ID: "1000aa", Capability: "brightness", Value: 42.5}, true, false},
		{"integer", "oikade/v1/sonoff/1000aa/set", ` {"brightness": 3} `,
			SetCommand{ID: "1000aa", Capability: "brightness", Value: 3.0}, true, false},

		{"state topic", "oikade/v1/sonoff/1000aa/state", `{"on":true}`, SetCommand{}, false, false},
		{"status topic", "oikade/v1/sonoff/status", `{}`, SetCommand{}, false, false},
		{"other root", "oikade/v1/other/1000aa/set", `{"on":true}`, SetCommand{}, false, false},
		{"root prefix", "oikade/v1/sonoff2/1000aa/set", `{"on":true}`, SetCommand{}, false, false},
		{"no id", "oikade/v1/sonoff//set", `{"on":true}`, SetCommand{}, false, false},
		{"nested id", "oikade/v1/sonoff/a/b/set", `{"on":true}`, SetCommand{}, false, false},
		{"set suffix only", "oikade/v1/sonoff/set", `{"on":true}`, SetCommand{}, false, false},

		{"not JSON", "oikade/v1/sonoff/1000aa/set", `on`, SetCommand{}, true, true},
		{"trailing data", "oikade/v1/sonoff/1000aa/set", `{"on":true} x`, SetCommand{}, true, true},
		{"empty", "oikade/v1/sonoff/1000aa/set", ``, SetCommand{}, true, true},
		{"null", "oikade/v1/sonoff/1000aa/set", `null`, SetCommand{}, true, true},
		{"array", "oikade/v1/sonoff/1000aa/set", `[true]`, SetCommand{}, true, true},
		{"no capability", "oikade/v1/sonoff/1000aa/set", `{}`, SetCommand{}, true, true},
		{"repeated capability", "oikade/v1/sonoff/1000aa/set", `{"on":true,"on":false}`,
			SetCommand{}, true, true},
		{"unclosed", "oikade/v1/sonoff/1000aa/set", `{"on":true`, SetCommand{}, true, true},
		{"two capabilities", "oikade/v1/sonoff/1000aa/set", `{"on":true,"brightness":1}`,
			SetCommand{}, true, true},
		{"empty capability", "oikade/v1/sonoff/1000aa/set", `{"":true}`, SetCommand{}, true, true},
		{"string value", "oikade/v1/sonoff/1000aa/set", `{"on":"true"}`, SetCommand{}, true, true},
		{"null value", "oikade/v1/sonoff/1000aa/set", `{"on":null}`, SetCommand{}, true, true},
		{"object value", "oikade/v1/sonoff/1000aa/set", `{"on":{}}`, SetCommand{}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, ok, err := r.ParseSet(tc.topic, []byte(tc.payload))
			if ok != tc.ok || (err != nil) != tc.err || !reflect.DeepEqual(cmd, tc.want) {
				t.Errorf("ParseSet = %+v, %v, %v; want %+v, %v, error %v", cmd, ok, err, tc.want, tc.ok, tc.err)
			}
		})
	}
}

func TestSetCommandPayloadRoundTrips(t *testing.T) {
	r := NewRoot("sonoff")
	for _, want := range []SetCommand{
		{ID: "1000aa", Capability: "on", Value: true},
		{ID: "1000aa", Capability: "brightness", Value: 42.5},
	} {
		payload, err := want.Payload()
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := r.ParseSet(r.Set(want.ID), payload)
		if !ok || err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseSet(Payload(%+v)) = %+v, %v, %v", want, got, ok, err)
		}
	}
	if _, err := (SetCommand{ID: "1000aa", Value: true}).Payload(); err == nil {
		t.Error("Payload without a capability should fail")
	}
}

func TestDiscoveredTellsNoneFromEmpty(t *testing.T) {
	for _, tc := range []struct {
		devices Devices
		want    string
	}{
		{Devices{Devices: []DeviceReport{}}, `{"devices":[],"undeclared":0}`},
		{
			Devices{Devices: []DeviceReport{}, Discovered: []DiscoveredDevice{}},
			`{"devices":[],"undeclared":0,"discovered":[]}`,
		},
	} {
		got, err := json.Marshal(tc.devices)
		if err != nil || string(got) != tc.want {
			t.Errorf("encoded %s, %v; want %s", got, err, tc.want)
		}
		var back Devices
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatal(err)
		}
		if (back.Discovered == nil) != (tc.devices.Discovered == nil) {
			t.Errorf("%s decoded discovered as %#v", got, back.Discovered)
		}
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"1000aabbcc":                           true,
		"3f2a9c71-0b1e-4c2d-9a8f-5e6d7c8b9a01": true,
		"0x00158d0001a2b3c4":                   true,
		"a.b_c-d":                              true,
		strings.Repeat("a", 63):                true,
		"":                                     false,
		"-lead":                                false,
		"a/b":                                  false,
		"a+b":                                  false,
		"a b":                                  false,
		strings.Repeat("a", 64):                false,
	} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}
