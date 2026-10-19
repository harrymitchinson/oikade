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
		{`{"switch.on":true}`, func() ([]byte, error) { return json.Marshal(State{"switch.on": true}) }},
		{`{"switch.on":false}`, SetCommand{ID: "1", Capability: "switch.on", Value: false}.Payload},
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
	r := NewRoot("home", "sonoff")
	for got, want := range map[string]string{
		string(r):                "oikade/v1/home/sonoff",
		r.Status():               "oikade/v1/home/sonoff/status",
		r.Devices():              "oikade/v1/home/sonoff/devices",
		r.State("1000aa"):        "oikade/v1/home/sonoff/1000aa/state",
		r.Availability("1000aa"): "oikade/v1/home/sonoff/1000aa/availability",
		r.Set("1000aa"):          "oikade/v1/home/sonoff/1000aa/set",
		r.SetFilter():            "oikade/v1/home/sonoff/+/set",
		r.Filter():               "oikade/v1/home/sonoff/#",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestParseRoot(t *testing.T) {
	if r, err := ParseRoot("oikade/v1/home/sonoff"); err != nil || r != NewRoot("home", "sonoff") {
		t.Errorf("ParseRoot(valid) = %q, %v", r, err)
	}
	for _, bad := range []string{
		"",
		"oikade/v1",
		"oikade/v1/",
		"oikade/v1/home",
		"oikade/v1/home/",
		"oikade/v1//sonoff",
		"oikade/v1/home/sonoff/",
		"oikade/v1/home/sonoff/extra",
		"oikade/v2/home/sonoff",
		"/oikade/v1/home/sonoff",
		"zigbee2mqtt/home/sonoff",
		"oikade/v1/+/sonoff",
		"oikade/v1/home/#",
		"oikade/v1/home/so+noff",
		"oikade/v1/home/so\x00noff",
	} {
		if r, err := ParseRoot(bad); err == nil {
			t.Errorf("ParseRoot(%q) = %q, want an error", bad, r)
		}
	}
}

func TestParseSet(t *testing.T) {
	r := NewRoot("home", "sonoff")
	for _, tc := range []struct {
		name    string
		topic   string
		payload string
		want    SetCommand
		ok      bool
		err     bool
	}{
		{"bool", "oikade/v1/home/sonoff/1000aa/set", `{"switch.on":true}`,
			SetCommand{ID: "1000aa", Capability: "switch.on", Value: true}, true, false},
		{"number", "oikade/v1/home/sonoff/1000aa/set", `{"light.level":42.5}`,
			SetCommand{ID: "1000aa", Capability: "light.level", Value: 42.5}, true, false},
		{"integer", "oikade/v1/home/sonoff/1000aa/set", ` {"light.level": 3} `,
			SetCommand{ID: "1000aa", Capability: "light.level", Value: 3.0}, true, false},

		{"state topic", "oikade/v1/home/sonoff/1000aa/state", `{"switch.on":true}`, SetCommand{}, false, false},
		{"status topic", "oikade/v1/home/sonoff/status", `{}`, SetCommand{}, false, false},
		{"other root", "oikade/v1/home/other/1000aa/set", `{"switch.on":true}`, SetCommand{}, false, false},
		{"root prefix", "oikade/v1/home/sonoff2/1000aa/set", `{"switch.on":true}`, SetCommand{}, false, false},
		{"no id", "oikade/v1/home/sonoff//set", `{"switch.on":true}`, SetCommand{}, false, false},
		{"nested id", "oikade/v1/home/sonoff/a/b/set", `{"switch.on":true}`, SetCommand{}, false, false},
		{"set suffix only", "oikade/v1/home/sonoff/set", `{"switch.on":true}`, SetCommand{}, false, false},

		{"not JSON", "oikade/v1/home/sonoff/1000aa/set", `on`, SetCommand{}, true, true},
		{"trailing data", "oikade/v1/home/sonoff/1000aa/set", `{"switch.on":true} x`, SetCommand{}, true, true},
		{"empty", "oikade/v1/home/sonoff/1000aa/set", ``, SetCommand{}, true, true},
		{"null", "oikade/v1/home/sonoff/1000aa/set", `null`, SetCommand{}, true, true},
		{"array", "oikade/v1/home/sonoff/1000aa/set", `[true]`, SetCommand{}, true, true},
		{"no capability", "oikade/v1/home/sonoff/1000aa/set", `{}`, SetCommand{}, true, true},
		{"repeated capability", "oikade/v1/home/sonoff/1000aa/set", `{"switch.on":true,"switch.on":false}`,
			SetCommand{}, true, true},
		{"unclosed", "oikade/v1/home/sonoff/1000aa/set", `{"switch.on":true`, SetCommand{}, true, true},
		{"two capabilities", "oikade/v1/home/sonoff/1000aa/set", `{"switch.on":true,"light.level":1}`,
			SetCommand{}, true, true},
		{"empty capability", "oikade/v1/home/sonoff/1000aa/set", `{"":true}`, SetCommand{}, true, true},
		{"string value", "oikade/v1/home/sonoff/1000aa/set", `{"switch.on":"true"}`, SetCommand{}, true, true},
		{"null value", "oikade/v1/home/sonoff/1000aa/set", `{"switch.on":null}`, SetCommand{}, true, true},
		{"object value", "oikade/v1/home/sonoff/1000aa/set", `{"switch.on":{}}`, SetCommand{}, true, true},
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
	r := NewRoot("home", "sonoff")
	for _, want := range []SetCommand{
		{ID: "1000aa", Capability: "switch.on", Value: true},
		{ID: "1000aa", Capability: "light.level", Value: 42.5},
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
