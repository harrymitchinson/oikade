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

package plugin

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
)

var operatorEnv = map[string]string{
	contractv1.EnvMQTTURL:     "mqtt://broker:1883",
	contractv1.EnvTopic:       "oikade/v1/home/sonoff",
	contractv1.EnvDevicesFile: "/etc/oikade/config/devices.json",
}

func envWith(overrides map[string]string) func(string) string {
	return func(name string) string {
		if v, ok := overrides[name]; ok {
			return v
		}
		return operatorEnv[name]
	}
}

func TestReadEnvironment(t *testing.T) {
	got, err := readEnvironment(envWith(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Environment{
		MQTTURL:     "mqtt://broker:1883",
		Root:        "oikade/v1/home/sonoff",
		DevicesFile: "/etc/oikade/config/devices.json",
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	got, err = readEnvironment(envWith(map[string]string{contractv1.EnvSecretsDir: "/etc/oikade/secrets"}))
	if err != nil || got.SecretsDir != "/etc/oikade/secrets" {
		t.Fatalf("got %+v, %v, want the secrets dir", got, err)
	}
}

func TestReadEnvironmentRejects(t *testing.T) {
	for _, tc := range []struct {
		name, variable, value string
	}{
		{"missing URL", contractv1.EnvMQTTURL, ""},
		{"missing devices file", contractv1.EnvDevicesFile, ""},
		{"missing topic", contractv1.EnvTopic, ""},
		{"another contract", contractv1.EnvTopic, "oikade/v2/home/x"},
		{"no plugin", contractv1.EnvTopic, "oikade/v1/home"},
		{"too deep", contractv1.EnvTopic, "oikade/v1/home/x/y"},
		{"wildcard", contractv1.EnvTopic, "oikade/v1/+/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readEnvironment(envWith(map[string]string{tc.variable: tc.value}))
			if err == nil || !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("got %v, want an error naming %s", err, tc.variable)
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadDevices(t *testing.T) {
	file := writeFile(t, t.TempDir(), "devices.json", `{"contract":1,"devices":[
		{"id":"1000aabbcc","name":"porch-light","displayName":"Porch light","room":"porch",
		 "parameters":{"capability":"light.on"}},
		{"id":"2","name":"hall","displayName":"hall"}]}`)
	got, err := ReadDevices(file)
	if err != nil {
		t.Fatal(err)
	}
	want := []contractv1.DeclaredDevice{
		{ID: "1000aabbcc", Name: "porch-light", DisplayName: "Porch light", Room: "porch",
			Parameters: map[string]string{"capability": "light.on"}},
		{ID: "2", Name: "hall", DisplayName: "hall"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadDevicesRejects(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, content, want string
	}{
		{"another contract", `{"contract":2,"devices":[]}`, "contract 2"},
		{"no devices", `{"contract":1}`, "devices must be a list"},
		{"no display name", `{"contract":1,"devices":[{"id":"x","name":"x"}]}`, "devices[0]: displayName"},
		{
			"parameter not a string",
			`{"contract":1,"devices":[{"id":"x","name":"x","displayName":"x","parameters":{"a":1}}]}`,
			"parameters",
		},
		{"not JSON", `nope`, "invalid character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadDevices(writeFile(t, dir, "devices.json", tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if _, err := ReadDevices(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("read a missing file")
	}
}

func TestReadSecret(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "1000aabbcc", "lan-key")
	env := Environment{SecretsDir: dir}

	if v, ok, err := ReadSecret(env, "1000aabbcc"); v != "lan-key" || !ok || err != nil {
		t.Fatalf("got %q, %v, %v, want the key", v, ok, err)
	}
	if _, ok, err := ReadSecret(env, "missing"); ok || err != nil {
		t.Fatalf("got %v, %v for a missing key, want absent", ok, err)
	}
	if _, ok, err := ReadSecret(Environment{}, "1000aabbcc"); ok || err != nil {
		t.Fatalf("got %v, %v without a Secret, want absent", ok, err)
	}
	for _, key := range []string{"../1000aabbcc", "", ".", ".."} {
		if _, ok, err := ReadSecret(env, key); ok || err == nil {
			t.Fatalf("got %v, %v for key %q, want it refused", ok, err, key)
		}
	}
}

func TestRelayCapability(t *testing.T) {
	for params, want := range map[string]string{"": "switch.on", "light.on": "light.on", "outlet.on": "outlet.on"} {
		got, err := RelayCapability(map[string]string{"capability": params})
		if got != want || err != nil {
			t.Fatalf("got %q, %v for %q, want %q", got, err, params, want)
		}
	}
	if got, err := RelayCapability(nil); got != "switch.on" || err != nil {
		t.Fatalf("got %q, %v without parameters, want switch.on", got, err)
	}
	_, err := RelayCapability(map[string]string{"capability": "fan.on"})
	want := "parameters.capability must be one of switch.on, light.on, outlet.on, got fan.on"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}
