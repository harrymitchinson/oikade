// Package v1 is the oikade/v1 plugin contract (docs/plugin-contract.md) as Go types.
//
// Oikade's decoder, its devices.json renderer and the Go plugin SDK all use these types, so the
// bytes on the wire can't drift between them. It imports nothing from api/ or internal/, because
// the api packages import it.
package v1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Contract is the contract version, as carried in Status and DevicesFile.
const Contract = 1

// The values of the status and availability topics.
const (
	Online  = "online"
	Offline = "offline"
)

// The environment the operator sets on a plugin pod.
const (
	EnvMQTTURL     = "OIKADE_MQTT_URL"
	EnvTopic       = "OIKADE_TOPIC"
	EnvDevicesFile = "OIKADE_DEVICES_FILE"
	// EnvSecretsDir is set only when the Plugin has a spec.secretRef.
	EnvSecretsDir = "OIKADE_SECRETS_DIR"
)

const prefix = "oikade/v1/"

// Root is a plugin's topic root, oikade/v1/<namespace>/<plugin>.
type Root string

// NewRoot returns the root of the Plugin with this namespace and name.
func NewRoot(namespace, plugin string) Root {
	return Root(prefix + namespace + "/" + plugin)
}

// ParseRoot checks a root read from the environment. It rejects anything but
// oikade/v1/<namespace>/<plugin>, with segments that are non-empty and free of MQTT wildcards.
func ParseRoot(s string) (Root, error) {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return "", fmt.Errorf("topic root %q does not start with %s", s, prefix)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("topic root %q is not %s<namespace>/<plugin>", s, prefix)
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, "+#\x00") {
			return "", fmt.Errorf("topic root %q is not %s<namespace>/<plugin>", s, prefix)
		}
	}
	return Root(s), nil
}

// Status is the status topic.
func (r Root) Status() string { return string(r) + "/status" }

// Devices is the devices topic.
func (r Root) Devices() string { return string(r) + "/devices" }

// State is a device's state topic.
func (r Root) State(id string) string { return string(r) + "/" + id + "/state" }

// Availability is a device's availability topic.
func (r Root) Availability(id string) string { return string(r) + "/" + id + "/availability" }

// Set is a device's set topic.
func (r Root) Set(id string) string { return string(r) + "/" + id + "/set" }

// SetFilter matches every device's set topic.
func (r Root) SetFilter() string { return string(r) + "/+/set" }

// Filter matches every topic under the root.
func (r Root) Filter() string { return string(r) + "/#" }

// Status is the retained status payload, and the Last Will when State is Offline.
type Status struct {
	State    string `json:"state"`
	Contract int    `json:"contract"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// ValidID reports whether id is a valid device ID: 1 to 63 letters, digits, '_', '.' or '-',
// starting with a letter or digit. It never contains a '/', so it fits in a topic.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Devices is the retained devices payload.
type Devices struct {
	Devices []DeviceReport `json:"devices"`
	// Undeclared counts devices the plugin can see that have no Device object. Oikade ignores it
	// when Discovered is set, and counts them from Discovered instead.
	Undeclared int `json:"undeclared"`
	// Discovered lists every device the plugin sees in its app, declared or not. Nil means the
	// plugin doesn't discover devices; an empty list means its app has none. A device leaving the
	// list means it was removed from its app, not that it's offline.
	Discovered []DiscoveredDevice `json:"discovered,omitzero"`
}

// DiscoveredDevice is one device the plugin sees in its app.
type DiscoveredDevice struct {
	// ID must stay the same across restarts of the plugin and of its hub or account, until the
	// device is reset or re-paired. See ValidID.
	ID           string   `json:"id"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// Name and Room are as the user named them in the device's app, empty if it has none.
	Name string `json:"name,omitempty"`
	Room string `json:"room,omitempty"`
}

// DeviceReport is one declared device in the devices payload.
type DeviceReport struct {
	ID    string `json:"id"`
	Found bool   `json:"found"`
	// Manufacturer, Model and Capabilities matter only when Found is true.
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// Error is a problem with the device's configuration, empty otherwise.
	Error string `json:"error,omitempty"`
}

// State is a device's current values: bool, or float64 within the capability's range. It's full, but
// for values a device has yet to report and can't be asked for.
type State map[string]any

// SetCommand is one capability of one device, as sent on <root>/<id>/set.
type SetCommand struct {
	ID         string
	Capability string
	// Value is a bool or a float64.
	Value any
}

// Payload is the command's set payload, {"<capability>": value}.
func (c SetCommand) Payload() ([]byte, error) {
	if c.Capability == "" {
		return nil, errors.New("set command has no capability")
	}
	return json.Marshal(map[string]any{c.Capability: c.Value})
}

// ParseSet parses a message on <root>/<id>/set. ok is false for any other topic; err is set for a
// payload that isn't exactly one capability with a bool or number value. Whether the capability
// exists and the value is in range is left to the caller.
func (r Root) ParseSet(topic string, payload []byte) (cmd SetCommand, ok bool, err error) {
	rest, found := strings.CutPrefix(topic, string(r)+"/")
	if !found {
		return SetCommand{}, false, nil
	}
	id, found := strings.CutSuffix(rest, "/set")
	if !found || id == "" || strings.ContainsAny(id, "/+#") {
		return SetCommand{}, false, nil
	}
	name, v, n, err := onlyMember(payload)
	if err != nil {
		return SetCommand{}, true, fmt.Errorf("set payload is not a JSON object: %w", err)
	}
	if n != 1 {
		return SetCommand{}, true, fmt.Errorf("set payload has %d capabilities, want 1", n)
	}
	switch v.(type) {
	case bool, float64:
	default:
		return SetCommand{}, true, fmt.Errorf("set value of %s is %s, want a bool or a number", name, kind(v))
	}
	if name == "" {
		return SetCommand{}, true, errors.New("set payload has an empty capability")
	}
	cmd = SetCommand{ID: id, Capability: name, Value: v}
	return cmd, true, nil
}

// onlyMember reads a JSON object, returning its last member and how many it has. Unlike decoding
// into a map, it counts a repeated name twice.
func onlyMember(payload []byte) (name string, value any, n int, err error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return "", nil, 0, errors.New("want an object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return "", nil, 0, err
		}
		name, _ = t.(string)
		if err := dec.Decode(&value); err != nil {
			return "", nil, 0, err
		}
		n++
	}
	if _, err := dec.Token(); err != nil {
		return "", nil, 0, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", nil, 0, errors.New("data after the object")
	}
	return name, value, n, nil
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// DevicesFile is devices.json, written by the operator and read by the plugin.
type DevicesFile struct {
	Contract int              `json:"contract"`
	Devices  []DeclaredDevice `json:"devices"`
}

// DeclaredDevice is a Device declared for the plugin.
type DeclaredDevice struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Room        string `json:"room,omitempty"`
	// Parameters are passed through from Device.spec.plugin.parameters without validation.
	Parameters map[string]string `json:"parameters,omitempty"`
}
