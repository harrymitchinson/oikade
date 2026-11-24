package ident

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// The kinds a ref may have besides those whose names Oikade owns. Device is a capability's kind
// too, as device/desk-plug/on, and on its own the claim source of a change made on the device.
const (
	// Hardware is a device's hardware: hardware/<plugin>/<id>, by the ID its driver gives it.
	Hardware Kind = "hardware"
	// Matter is the claim source of a change a Matter controller made.
	Matter Kind = "matter"
	// Command is the claim source of a change oikade set made.
	Command Kind = "command"
)

// Ref is the one string that names a thing a home is made of, the same in every command, --json,
// log line, metric label and error: its kind, then its name and any parts, each a URI path
// segment, such as case/desk-plug/on/night. Every ref is a relative URI reference.
type Ref string

// refShape is what a kind's ref holds after its kind, by the parts' names. A kind with several
// shapes takes any of them.
var refShape = map[Kind][][]string{
	Device:   {{}, {partDevice}, {partDevice, partCapability}},
	Hardware: {{"PLUGIN", "ID"}},
	Room:     {{"ROOM"}, {"ROOM", partCapability}},
	Group:    {{"GROUP"}, {"GROUP", partCapability}},
	Case:     {{partDevice, partCapability, "CASE"}},
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
		e := url.PathEscape(p)
		if e == "." || e == ".." {
			e = strings.ReplaceAll(e, ".", "%2E") // a dot segment would be resolved away, as in a URI
		}
		b.WriteString(e)
	}
	return Ref(b.String())
}

// DeviceRef is a device's ref, such as device/desk-plug.
func DeviceRef(name string) Ref { return NewRef(Device, name) }

// HardwareRef is a device's hardware's ref, such as hardware/overkiz/io:%2F%2F1234%2F5: the
// plugin's name and the ID it knows the device by, percent-encoded where it isn't safe in a ref.
func HardwareRef(plugin, id string) Ref { return NewRef(Hardware, plugin, id) }

// CapabilityRef is a device's capability's ref, such as device/desk-plug/on.
func CapabilityRef(device, capability string) Ref { return NewRef(Device, device, capability) }

// CaseRef is a decision's case's ref, such as case/desk-plug/on/night.
func CaseRef(device, capability, name string) Ref { return NewRef(Case, device, capability, name) }

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
