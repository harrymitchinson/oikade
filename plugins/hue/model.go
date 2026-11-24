package hue

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"oikade.dev/contract/v1"
)

// The resource types the plugin reads. The bridge has many more, such as scenes, which it skips.
const (
	typeDevice       = "device"
	typeRoom         = "room"
	typeLight        = "light"
	typeConnectivity = "zigbee_connectivity"
	typeMotion       = "motion"
	typeTemperature  = "temperature"
	typePower        = "device_power"
	typeBridge       = "bridge"
)

var read = []string{typeDevice, typeRoom, typeLight, typeConnectivity, typeMotion, typeTemperature, typePower}

// structural are the types a change to which can change what devices there are, or their names
// and rooms, so the plugin reads everything again.
var structural = []string{typeDevice, typeRoom}

// ref is a link from one resource to another.
type ref struct {
	RID   string `json:"rid"`
	RType string `json:"rtype"`
}

// resource is one of the bridge's resources, of a type the plugin reads. An event decodes into the
// one it changes, which keeps whatever the event leaves out, since an event carries only what
// changed.
type resource struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// A device's and a room's.
	Metadata *struct {
		Name string `json:"name"`
	} `json:"metadata,omitempty"`
	Services []ref `json:"services,omitempty"`
	// A device's.
	ProductData *struct {
		ModelID      string `json:"model_id"`
		Manufacturer string `json:"manufacturer_name"`
		Archetype    string `json:"product_archetype"`
		Software     string `json:"software_version"`
	} `json:"product_data,omitempty"`
	// A room's: its devices.
	Children []ref `json:"children,omitempty"`
	// A light's.
	On *struct {
		On bool `json:"on"`
	} `json:"on,omitempty"`
	Dimming *struct {
		Brightness float64 `json:"brightness"`
	} `json:"dimming,omitempty"`
	// A zigbee_connectivity's.
	Status string `json:"status,omitempty"`
	MAC    string `json:"mac_address,omitempty"`
	// A sensor's: motion, temperature, and device_power.
	Enabled *bool `json:"enabled,omitempty"`
	Motion  *struct {
		Motion bool `json:"motion"`
		Valid  bool `json:"motion_valid"`
		Report *struct {
			Motion bool `json:"motion"`
		} `json:"motion_report,omitempty"`
	} `json:"motion,omitempty"`
	Temperature *struct {
		Temperature float64 `json:"temperature"`
		Valid       bool    `json:"temperature_valid"`
		Report      *struct {
			Temperature float64 `json:"temperature"`
		} `json:"temperature_report,omitempty"`
	} `json:"temperature,omitempty"`
	PowerState *struct {
		BatteryLevel *float64 `json:"battery_level"`
	} `json:"power_state,omitempty"`
}

// kind is a resource's ID and type, read before the rest to see whether it's one the plugin reads.
type kind struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// device is one device the plugin reports, by the IDs of the resources it's made of.
type device struct {
	id string
	// hue is the Hue device's own resource ID, and the others its services', "" if it has none.
	hue, light, connectivity, motion, temperature, power string
	info                                                 contract.DiscoveredDevice
	// online and values are what was last published, and published whether anything has been.
	online    bool
	values    contract.State
	published bool
}

// home is what the bridge said in its last read, with every event since.
type home struct {
	resources map[string]*resource
	devices   map[string]*device
	// owner is a device by the ID of each resource it's made of.
	owner map[string]*device
}

// skip is a device the plugin doesn't report, and why: the log names it once.
type skip struct {
	model, name, reason string
}

// build reads every resource into a home. It returns the devices it skips, but for the bridge
// itself, which reports nothing.
func build(raw []json.RawMessage) (*home, []skip, error) {
	h := &home{resources: map[string]*resource{}, devices: map[string]*device{}, owner: map[string]*device{}}
	for _, r := range raw {
		var k kind
		if err := json.Unmarshal(r, &k); err != nil {
			return nil, nil, fmt.Errorf("could not read a resource: %w", err)
		}
		if k.ID == "" || !slices.Contains(read, k.Type) {
			continue
		}
		res := &resource{}
		if err := json.Unmarshal(r, res); err != nil {
			return nil, nil, fmt.Errorf("could not read %s %s: %w", k.Type, k.ID, err)
		}
		h.resources[k.ID] = res
	}
	rooms := map[string]string{}
	for _, id := range slices.Sorted(maps.Keys(h.resources)) {
		r := h.resources[id]
		if r.Type != typeRoom || r.Metadata == nil {
			continue
		}
		for _, c := range r.Children {
			if c.RType == typeDevice {
				rooms[c.RID] = r.Metadata.Name
			}
		}
	}
	var skipped []skip
	for _, id := range slices.Sorted(maps.Keys(h.resources)) {
		r := h.resources[id]
		if r.Type != typeDevice {
			continue
		}
		d, why := h.device(r)
		if why == "" {
			if other, ok := h.devices[d.id]; ok {
				why = fmt.Sprintf("it has the same MAC as %s", other.hue)
			}
		}
		if why != "" {
			if !slices.ContainsFunc(r.Services, func(s ref) bool { return s.RType == typeBridge }) {
				skipped = append(skipped, skip{model: r.model(), name: r.name(), reason: why})
			}
			continue
		}
		d.info.Room = rooms[r.ID]
		h.devices[d.id] = d
		for _, rid := range []string{d.hue, d.light, d.connectivity, d.motion, d.temperature, d.power} {
			if rid != "" {
				h.owner[rid] = d
			}
		}
	}
	return h, skipped, nil
}

// device reads a Hue device, or says why the plugin doesn't report it.
func (h *home) device(r *resource) (*device, string) {
	services := map[string][]string{}
	for _, s := range r.Services {
		if h.resources[s.RID] != nil && h.resources[s.RID].Type == s.RType {
			services[s.RType] = append(services[s.RType], s.RID)
		}
	}
	lights, motions := services[typeLight], services[typeMotion]
	switch {
	case len(lights) == 0 && len(motions) == 0:
		return nil, "it has no light or motion sensor"
	case len(lights) > 1:
		return nil, fmt.Sprintf("it has %d lights", len(lights))
	case len(motions) > 1:
		return nil, fmt.Sprintf("it has %d motion sensors", len(motions))
	case len(services[typeConnectivity]) != 1:
		return nil, "it has no Zigbee MAC"
	}
	d := &device{hue: r.ID, connectivity: services[typeConnectivity][0]}
	id, ok := macID(h.resources[d.connectivity].MAC)
	if !ok {
		return nil, fmt.Sprintf("its Zigbee MAC %q isn't one", h.resources[d.connectivity].MAC)
	}
	d.id = id
	first := func(t string) string {
		if s := services[t]; len(s) > 0 {
			return s[0]
		}
		return ""
	}
	var caps []string
	deviceType := ""
	if d.light = first(typeLight); d.light != "" {
		caps = append(caps, contract.On)
		dimmable := h.resources[d.light].Dimming != nil
		if dimmable {
			caps = append(caps, contract.Brightness)
		}
		deviceType = lightType(r.archetype(), dimmable)
	}
	if d.motion = first(typeMotion); d.motion != "" {
		caps = append(caps, contract.Occupied)
		if d.temperature = first(typeTemperature); d.temperature != "" {
			caps = append(caps, contract.Temperature)
		}
		if d.power = first(typePower); d.power != "" {
			caps = append(caps, contract.Battery)
		}
	}
	slices.Sort(caps)
	d.info = contract.DiscoveredDevice{
		ID: id, Capabilities: caps, DeviceType: deviceType, Name: r.name(),
	}
	if p := r.ProductData; p != nil {
		d.info.Manufacturer, d.info.Model, d.info.FirmwareVersion = p.Manufacturer, p.ModelID, p.Software
	}
	return d, ""
}

// lightType is what a light is, by its archetype in the Hue app: a plug is a plug, and anything
// else a light.
func lightType(archetype string, dimmable bool) string {
	switch {
	case archetype == "plug" && dimmable:
		return contract.DimmablePlugInUnit
	case archetype == "plug":
		return contract.OnOffPlugInUnit
	case dimmable:
		return contract.DimmableLight
	}
	return contract.OnOffLight
}

// macID is a device's ID from its Zigbee MAC, such as 7c:c6:b6:ff:fe:cb:18:14, as Zigbee2MQTT
// writes it: 0x7cc6b6fffecb1814.
func macID(mac string) (string, bool) {
	raw, err := hex.DecodeString(strings.ReplaceAll(mac, ":", ""))
	if err != nil || len(raw) != 8 || strings.Count(mac, ":") != 7 {
		return "", false
	}
	return "0x" + hex.EncodeToString(raw), true
}

func (r *resource) name() string {
	if r.Metadata == nil {
		return ""
	}
	return r.Metadata.Name
}

func (r *resource) model() string {
	if r.ProductData == nil {
		return ""
	}
	return r.ProductData.ModelID
}

func (r *resource) archetype() string {
	if r.ProductData == nil {
		return ""
	}
	return r.ProductData.Archetype
}

// online is whether the bridge can reach the device over Zigbee.
func (h *home) online(d *device) bool {
	c := h.resources[d.connectivity]
	return c != nil && c.Status == "connected"
}

// values are the device's values, leaving out any its sensor doesn't have yet, such as a motion
// sensor that's turned off.
func (h *home) values(d *device) contract.State {
	s := contract.State{}
	if l := h.resources[d.light]; l != nil {
		if l.On != nil {
			s[contract.On] = l.On.On
		}
		if l.Dimming != nil {
			s[contract.Brightness] = l.Dimming.Brightness
		}
	}
	if m := h.resources[d.motion]; m != nil && m.Motion != nil && m.Motion.Valid && enabled(m) {
		s[contract.Occupied] = m.Motion.Motion
		if m.Motion.Report != nil {
			s[contract.Occupied] = m.Motion.Report.Motion
		}
	}
	if t := h.resources[d.temperature]; t != nil && t.Temperature != nil && t.Temperature.Valid && enabled(t) {
		s[contract.Temperature] = t.Temperature.Temperature
		if t.Temperature.Report != nil {
			s[contract.Temperature] = t.Temperature.Report.Temperature
		}
	}
	if p := h.resources[d.power]; p != nil && p.PowerState != nil && p.PowerState.BatteryLevel != nil {
		s[contract.Battery] = *p.PowerState.BatteryLevel
	}
	return s
}

func enabled(r *resource) bool { return r.Enabled == nil || *r.Enabled }

// apply decodes a resource's change into it, and returns the device it's part of, nil if none. A
// change to a resource the plugin doesn't follow changes nothing.
func (h *home) apply(raw json.RawMessage) (*device, error) {
	var k kind
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("could not read a change: %w", err)
	}
	r := h.resources[k.ID]
	if r == nil || r.Type != k.Type {
		return nil, nil
	}
	// Into a copy, so a change that can't be read leaves the resource as it was.
	next := clone(r)
	if err := json.Unmarshal(raw, next); err != nil {
		return nil, fmt.Errorf("could not read a change to %s %s: %w", k.Type, k.ID, err)
	}
	*r = *next
	return h.owner[k.ID], nil
}

// clone is a deep copy of r, through JSON, which every field of it survives.
func clone(r *resource) *resource {
	raw, err := json.Marshal(r)
	if err != nil {
		panic(fmt.Sprintf("hue: a resource doesn't encode: %v", err))
	}
	c := &resource{}
	if err := json.Unmarshal(raw, c); err != nil {
		panic(fmt.Sprintf("hue: a resource doesn't decode: %v", err))
	}
	return c
}
