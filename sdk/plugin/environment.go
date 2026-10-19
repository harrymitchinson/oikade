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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/capability"
)

// Environment is where the operator tells a plugin to connect and what to serve.
type Environment struct {
	// MQTTURL is the broker, such as mqtt://oikade-mosquitto.oikade-system.svc:1883.
	MQTTURL string
	Root    contractv1.Root
	// DevicesFile is the path of devices.json.
	DevicesFile string
	// SecretsDir is the mounted user Secret, empty when the Plugin has no spec.secretRef.
	SecretsDir string
}

// ReadEnvironment reads the environment the operator sets on a plugin pod.
func ReadEnvironment() (Environment, error) {
	return readEnvironment(os.Getenv)
}

func readEnvironment(getenv func(string) string) (Environment, error) {
	var missing error
	required := func(name string) string {
		v := getenv(name)
		if v == "" && missing == nil {
			missing = fmt.Errorf("%s is not set; the Oikade operator sets it", name)
		}
		return v
	}
	env := Environment{
		MQTTURL:     required(contractv1.EnvMQTTURL),
		DevicesFile: required(contractv1.EnvDevicesFile),
		SecretsDir:  getenv(contractv1.EnvSecretsDir),
	}
	topic := required(contractv1.EnvTopic)
	if missing != nil {
		return Environment{}, missing
	}
	root, err := contractv1.ParseRoot(topic)
	if err != nil {
		return Environment{}, fmt.Errorf("%s: %w", contractv1.EnvTopic, err)
	}
	env.Root = root
	return env, nil
}

// ReadDevices reads and checks devices.json.
func ReadDevices(file string) ([]contractv1.DeclaredDevice, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var f struct {
		Contract int                          `json:"contract"`
		Devices  *[]contractv1.DeclaredDevice `json:"devices"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if f.Contract != contractv1.Contract {
		return nil, fmt.Errorf("%s: contract %d is not supported; this SDK speaks contract %d",
			file, f.Contract, contractv1.Contract)
	}
	if f.Devices == nil {
		return nil, fmt.Errorf("%s: devices must be a list", file)
	}
	for i, d := range *f.Devices {
		for _, field := range [][2]string{{"id", d.ID}, {"name", d.Name}, {"displayName", d.DisplayName}} {
			if field[1] == "" {
				return nil, fmt.Errorf("%s: devices[%d]: %s must be a non-empty string", file, i, field[0])
			}
		}
	}
	return *f.Devices, nil
}

// ReadSecret reads one key of the mounted user Secret, by convention the device id. ok is false
// when the Plugin has no Secret or the key is missing. Keys containing '/' or equal to "." or ".."
// are refused, because they could name the directory or its parent. Never log the result.
func ReadSecret(env Environment, key string) (value string, ok bool, err error) {
	if key == "" || key == "." || key == ".." || strings.Contains(key, "/") {
		return "", false, fmt.Errorf("secret key %q is not a file name", key)
	}
	if env.SecretsDir == "" {
		return "", false, nil
	}
	raw, err := os.ReadFile(filepath.Join(env.SecretsDir, key))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// relayCapabilities are the capabilities a relay-like device may report (docs/plugin-contract.md,
// "Relay-like devices").
var relayCapabilities = []string{capability.SwitchOn, capability.LightOn, capability.OutletOn}

// RelayCapability picks the capability a relay reports from its parameters["capability"]:
// switch.on by default, or light.on or outlet.on. Report any error in the device's DeviceReport.
func RelayCapability(parameters map[string]string) (string, error) {
	wanted := parameters["capability"]
	if wanted == "" {
		return capability.SwitchOn, nil
	}
	if slices.Contains(relayCapabilities, wanted) {
		return wanted, nil
	}
	return "", fmt.Errorf("parameters.capability must be one of %s, got %s",
		strings.Join(relayCapabilities, ", "), wanted)
}
