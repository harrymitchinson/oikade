package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"oikade.dev/contract/v1"
)

// Environment is where a plugin is told to connect and what to serve.
type Environment struct {
	// MQTTURL is the broker an outside plugin speaks the contract on, such as mqtt://broker:1883:
	// the host's --broker. It's empty for a plugin your home imports, which takes a broker it uses,
	// such as Zigbee2MQTT's, in its own config.
	MQTTURL string
	// Root is the plugin's topic root, oikade/v1/<plugin>.
	Root contract.Root
	// DevicesFile is the path of a devices file, or empty for a plugin that reports what it finds.
	DevicesFile string
	// SecretsDir is the directory of secrets: what the plugin's setup kept, under the host.
	SecretsDir string
}

// ReadEnvironment reads an outside plugin's environment, the OIKADE_ variables contract/v1 names.
// OIKADE_MQTT_URL and OIKADE_TOPIC are required, and the topic must be a valid root.
func ReadEnvironment() (Environment, error) {
	return readEnvironment(os.Getenv)
}

func readEnvironment(getenv func(string) string) (Environment, error) {
	var missing error
	required := func(name string) string {
		v := getenv(name)
		if v == "" && missing == nil {
			missing = fmt.Errorf("%s is not set", name)
		}
		return v
	}
	env := Environment{
		MQTTURL:     required(contract.EnvMQTTURL),
		DevicesFile: getenv(contract.EnvDevicesFile),
		SecretsDir:  getenv(contract.EnvSecretsDir),
	}
	topic := required(contract.EnvTopic)
	if missing != nil {
		return Environment{}, missing
	}
	root, err := contract.ParseRoot(topic)
	if err != nil {
		return Environment{}, fmt.Errorf("%s: %w", contract.EnvTopic, err)
	}
	env.Root = root
	return env, nil
}

// ReadDevices reads and checks a devices file: contract 1, and devices that each have an id, a name
// and a displayName, with no id repeated.
func ReadDevices(file string) ([]contract.DeclaredDevice, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var f contract.DevicesFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if f.Contract != contract.Contract {
		return nil, fmt.Errorf("%s: contract %d is not supported; this SDK speaks contract %d",
			file, f.Contract, contract.Contract)
	}
	if f.Devices == nil {
		return nil, fmt.Errorf("%s: devices must be a list", file)
	}
	seen := map[string]bool{}
	for i, d := range f.Devices {
		for _, field := range [][2]string{{"id", d.ID}, {"name", d.Name}, {"displayName", d.DisplayName}} {
			if field[1] == "" {
				return nil, fmt.Errorf("%s: devices[%d]: %s must be a non-empty string", file, i, field[0])
			}
		}
		if seen[d.ID] {
			return nil, fmt.Errorf("%s: devices[%d]: id %s is repeated", file, i, d.ID)
		}
		seen[d.ID] = true
	}
	return f.Devices, nil
}

// ReadSecret reads one secret from env's secrets directory, by convention named for the device it's
// for. ok is false when there's no secrets directory or no such secret. An empty key, one with a
// '/', and "." and ".." are refused, since they could name the directory or its parent. Never log
// the result.
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
