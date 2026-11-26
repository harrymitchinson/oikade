package ident

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// The kinds a ref may have besides those whose names Oikade owns. Device on its own is the claim
// source of a change made on the device, and device/NAME a device with no hardware yet.
const (
	// Hardware is a device, by its hardware: hardware/<plugin>/<id>, by the ID its driver gives it,
	// and a capability of it is hardware/<plugin>/<id>/<capability>.
	Hardware Kind = "hardware"
	// Matter is the claim source of a change a Matter controller made.
	Matter Kind = "matter"
	// Command is the claim source of a change oikade set made.
	Command Kind = "command"
)

// Ref is the one string that names a thing a home is made of, the same in every command, --json,
// log line, metric label and error: its kind, then its name and any parts, each a URI path
// segment, such as case/hue/0x2/on/night. Every ref is a relative URI reference.
type Ref string

// refShape is what a kind's ref holds after its kind, by the parts' names. A kind with several
// shapes takes any of them.
var refShape = map[Kind][][]string{
	Device:   {{}, {partDevice}, {partDevice, partCapability}},
	Hardware: {{partPlugin, "ID"}, {partPlugin, "ID", partCapability}},
	Room:     {{"ROOM"}, {"ROOM", partCapability}},
	Group:    {{"GROUP"}, {"GROUP", partCapability}},
	Case:     {{partDevice, partCapability, "CASE"}, {partPlugin, "ID", partCapability, "CASE"}},
	Flow:     {{"FLOW"}},
	Signal:   {{"SIGNAL"}},
	Plugin:   {{"PLUGIN"}},
	Client:   {{"CLIENT"}},
	Matter:   {{}},
	Command:  {{}},
}

// A ref's parts that name a device and a capability. A capability's name follows the rule, but
// isn't a kind.
const (
	partDevice     = "DEVICE"
	partPlugin     = "PLUGIN"
	partCapability = "CAPABILITY"
)

// kinds lists the kinds in the order an error gives them.
var kinds = []Kind{Device, Hardware, Room, Group, Case, Flow, Signal, Plugin, Client, Matter, Command}

// NewRef is the ref of kind k with these parts. A part that isn't a name, as a driver's ID may not
// be, is percent-encoded.
func NewRef(k Kind, parts ...string) Ref {
	var b strings.Builder
	b.WriteString(string(k))
	for _, p := range parts {
		b.WriteByte('/')
		b.WriteString(segment(p))
	}
	return Ref(b.String())
}

func segment(p string) string {
	e := url.PathEscape(p)
	if e == "." || e == ".." {
		e = strings.ReplaceAll(e, ".", "%2E") // a dot segment would be resolved away, as in a URI
	}
	return e
}

// DeviceRef is the ref of the device whose key is key: its hardware's, such as hardware/hue/0x2, or
// device/NAME for one with no hardware yet, whose key is its name.
func DeviceRef(key string) Ref {
	if strings.Contains(key, "/") {
		return Ref(string(Hardware) + "/" + key)
	}
	return NewRef(Device, key)
}

// DeviceOf is the key of the device a ref of kind k names with its first parts, and the parts after
// them, or false for a ref that names no device: a hardware's, a device's or a case's.
func DeviceOf(k Kind, parts []string) (key string, rest []string, ok bool) {
	switch {
	case k == Hardware && len(parts) >= 2, k == Case && len(parts) == 4:
		return HardwareKey(parts[0], parts[1]), parts[2:], true
	case k == Device && len(parts) >= 1, k == Case && len(parts) == 3:
		return parts[0], parts[1:], true
	}
	return "", nil, false
}

// Simulator is the plugin name simulated hardware goes by, as in hardware/simulator/porch-light, so
// no plugin may have it.
const Simulator = "simulator"

// HardwareRef is a device's hardware's ref, such as hardware/overkiz/io:%2F%2F1234%2F5: the
// plugin's name and the ID it knows the device by, percent-encoded where it isn't safe in a ref.
func HardwareRef(plugin, id string) Ref { return NewRef(Hardware, plugin, id) }

// HardwareKey is how the host knows a plugin's device: its hardware's ref without the kind, such as
// overkiz/io:%2F%2F1234%2F5.
func HardwareKey(plugin, id string) string {
	return strings.TrimPrefix(HardwareRef(plugin, id).String(), string(Hardware)+"/")
}

// CapabilityRef is the ref of a capability of the device whose key is key, such as hardware/hue/0x2/on.
func CapabilityRef(key, capability string) Ref {
	return Ref(DeviceRef(key).String() + "/" + segment(capability))
}

// CaseRef is the ref of a decision's case of the device whose key is key, such as
// case/hue/0x2/on/night.
func CaseRef(key, capability, name string) Ref {
	device := key
	if !strings.Contains(key, "/") {
		device = segment(key)
	}
	return Ref(string(Case) + "/" + device + "/" + segment(capability) + "/" + segment(name))
}

func (r Ref) String() string { return string(r) }

// ParseRef reads a ref, refusing one whose kind isn't known, that has the wrong parts for its kind,
// or whose names don't follow the rule, and says what it should look like.
func ParseRef(s string) (Kind, []string, error) {
	segments := strings.Split(s, "/")
	k := Kind(segments[0])
	shapes, ok := refShape[k]
	if !ok {
		return "", nil, fmt.Errorf("%q isn't a ref: one starts with its kind, one of %s", s, joinKinds())
	}
	parts := make([]string, 0, len(segments)-1)
	for _, seg := range segments[1:] {
		p, err := url.PathUnescape(seg)
		if err != nil || p == "" {
			return "", nil, fmt.Errorf("%q isn't a ref: %q isn't a part of one", s, seg)
		}
		parts = append(parts, p)
	}
	i := slices.IndexFunc(shapes, func(shape []string) bool { return len(shape) == len(parts) })
	if i < 0 {
		return "", nil, fmt.Errorf("%q isn't a ref: a %s's is %s", s, k, shapesOf(k))
	}
	for j, p := range parts {
		if err := checkPart(shapes[i][j], p); err != nil {
			return "", nil, fmt.Errorf("%q isn't a ref: %w", s, err)
		}
	}
	return k, parts, nil
}

// checkPart refuses a part that should be a name and isn't. A driver's ID is the driver's.
func checkPart(part, p string) error {
	switch part {
	case "ID":
		return nil
	case partPlugin:
		return CheckName(Plugin, p)
	case partCapability:
		if !ValidName(p) {
			return fmt.Errorf("%q isn't a capability", p)
		}
		return nil
	}
	return CheckName(Kind(strings.ToLower(part)), p)
}

// shapesOf is how a kind's refs look, such as case/DEVICE/CAPABILITY/CASE.
func shapesOf(k Kind) string {
	out := make([]string, 0, len(refShape[k]))
	for _, shape := range refShape[k] {
		out = append(out, strings.Join(append([]string{string(k)}, shape...), "/"))
	}
	return strings.Join(out, ", ")
}

func joinKinds() string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return strings.Join(out, ", ")
}
