package zigbee2mqtt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"oikade.dev/internal/devicetype"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/matter/im"
	"oikade.dev/internal/matter/tlv"
	"oikade.dev/internal/matterbridge"
	"oikade.dev/internal/mqtt"
	"oikade.dev/internal/testkit"
)

const goldenDir = "testdata/golden"

// golden is what Oikade makes of a fixture: the device it reports, what each state payload
// changes, what each command would be as Oikade sends it, and the device on the Matter bridge.
type golden struct {
	Manufacturer string          `json:"manufacturer"`
	Model        string          `json:"model"`
	DeviceType   string          `json:"device_type"`
	Capabilities []string        `json:"capabilities"`
	Bindings     []goldenBinding `json:"bindings"`
	States       []goldenState   `json:"states"`
	Commands     []goldenCommand `json:"commands"`
	Matter       *goldenMatter   `json:"matter"`
}

type goldenBinding struct {
	Capability string `json:"capability"`
	Property   string `json:"property"`
	// Sets is the property a /set or /get names, where it isn't Property.
	Sets string `json:"sets,omitempty"`
	Get  bool   `json:"get"`
}

type goldenState struct {
	// Changes are the capabilities the payload changed, and their values.
	Changes map[string]any `json:"changes"`
	// Invalid are the capabilities whose property the payload has, but that Oikade can't read.
	Invalid map[string]string `json:"invalid,omitempty"`
}

type goldenCommand struct {
	// Values are each capability the command sets, as Oikade reads it, and the property's value as
	// Oikade would send it, which should be the command's own.
	Values  map[string]goldenValue `json:"values"`
	Invalid map[string]string      `json:"invalid,omitempty"`
	// Unmapped are the command's properties no capability has.
	Unmapped []string `json:"unmapped,omitempty"`
}

type goldenValue struct {
	Value any `json:"value"`
	Sends any `json:"sends"`
}

type goldenMatter struct {
	DeviceTypes []string `json:"device_types"`
	// Attributes are each cluster's attributes after every state, by cluster and attribute ID.
	Attributes map[string]map[string]any `json:"attributes"`
}

// TestGolden replays each fixture through the plugin's Instance and the Matter mapping, and
// compares what they make of it with its golden file. -update rewrites the golden files.
func TestGolden(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no fixtures in " + fixtureDir)
	}
	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(strings.TrimSuffix(name, ".json"), func(t *testing.T) {
			f, err := readFixture(path)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(replay(t, f)); err != nil {
				t.Fatal(err)
			}
			got := buf.Bytes()
			want := filepath.Join(goldenDir, name)
			if *testkit.Update {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(want, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			raw, err := os.ReadFile(want)
			if err != nil {
				t.Fatalf("%v: run go test ./plugins/zigbee2mqtt -run TestGolden -update", err)
			}
			if !bytes.Equal(raw, got) {
				t.Errorf("%s isn't what Oikade makes of %s now. If the change is right, run "+
					"go test ./plugins/zigbee2mqtt -run TestGolden -update and review the diff. Got:\n%s", want, path, got)
			}
		})
	}
}

// TestGoldensHaveFixtures fails on a golden file whose fixture has gone.
func TestGoldensHaveFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(fixtureDir, filepath.Base(p))); err != nil {
			t.Errorf("%s has no fixture: delete it", p)
		}
	}
}

func replay(t *testing.T, f fixture) golden {
	t.Helper()
	var d bridgeDevice
	if err := json.Unmarshal(f.Device, &d); err != nil {
		t.Fatal(err)
	}
	if d.IEEEAddress != fixtureIEEE || d.Definition == nil {
		t.Fatalf("a fixture's device is %s, with a definition", fixtureIEEE)
	}
	list, err := json.Marshal([]json.RawMessage{
		json.RawMessage(`{"ieee_address":"` + coordinatorIEEE + `","friendly_name":"Coordinator","type":"Coordinator","definition":null}`),
		f.Device,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	bus := mqtt.NewMemory()
	_ = bus.Publish(ctx, DefaultBaseTopic+"/bridge/devices", list, true)
	rec := newRecorder()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	i := &Instance{
		Key: dev(DefaultBaseTopic), BaseTopic: DefaultBaseTopic, Bus: bus, Sink: rec,
		Now: func() time.Time { return now }, Log: logging.Discard(),
		// No /get goes out while the fixture replays.
		getInterval: time.Hour,
	}
	device := dev("device")
	i.SetDevices(map[string]ident.ID{fixtureIEEE: device})
	if err := i.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.Stop)

	rec.mu.Lock()
	inventory := slices.Clone(rec.inventory)
	rec.mu.Unlock()
	if len(inventory) != 1 {
		t.Fatalf("inventory = %+v, want the fixture's device", inventory)
	}
	inv := inventory[0]
	g := golden{
		Manufacturer: inv.Manufacturer, Model: inv.Model, DeviceType: string(inv.DeviceType),
		Capabilities: orEmpty(inv.Capabilities), Bindings: []goldenBinding{},
		States: []goldenState{}, Commands: []goldenCommand{},
	}
	bs := bindings(d.Definition.Exposes)
	for _, b := range bs {
		gb := goldenBinding{Capability: b.capability, Property: b.property, Get: b.get}
		if s := b.setter().property; s != b.property {
			gb.Sets = s
		}
		g.Bindings = append(g.Bindings, gb)
	}

	for _, payload := range f.States {
		rec.mu.Lock()
		before := len(rec.changes)
		rec.mu.Unlock()
		_ = bus.Publish(ctx, DefaultBaseTopic+"/"+fixtureIEEE, payload, false)
		rec.mu.Lock()
		s := goldenState{Changes: map[string]any{}}
		for _, u := range rec.changes[before:] {
			s.Changes[u.Capability] = u.Value
		}
		rec.mu.Unlock()
		values := decodeObject(t, payload)
		for _, b := range bs {
			if raw, ok := values[b.property]; ok {
				if _, err := b.decode(raw); err != nil {
					s.Invalid = setDefault(s.Invalid)
					s.Invalid[b.capability] = err.Error()
				}
			}
		}
		g.States = append(g.States, s)
	}

	for _, payload := range f.Commands {
		values := decodeObject(t, payload)
		c := goldenCommand{Values: map[string]goldenValue{}}
		for prop, raw := range values {
			idx := slices.IndexFunc(bs, func(b binding) bool { return b.setter().property == prop })
			if idx < 0 {
				c.Unmapped = append(c.Unmapped, prop)
				continue
			}
			b := bs[idx].setter()
			v, err := b.decode(raw)
			if err != nil {
				c.Invalid = setDefault(c.Invalid)
				c.Invalid[b.capability] = err.Error()
				continue
			}
			c.Values[b.capability] = goldenValue{Value: v, Sends: b.encode(v)}
		}
		slices.Sort(c.Unmapped)
		g.Commands = append(g.Commands, c)
	}

	m := matterbridge.Map(matterbridge.Device{
		Key: device, Endpoint: 3, Name: device.Name, Capabilities: inv.Capabilities,
		Type: devicetype.OnOffOf("", inv.DeviceType, inv.Capabilities),
	}, func(string, any) {})
	if m != nil {
		rec.mu.Lock()
		for _, u := range rec.changes {
			m.Update(u.Capability, u.Value)
		}
		rec.mu.Unlock()
		g.Matter = &goldenMatter{DeviceTypes: []string{}, Attributes: map[string]map[string]any{}}
		for _, dt := range m.Types {
			g.Matter.DeviceTypes = append(g.Matter.DeviceTypes, fmt.Sprintf("0x%04x", dt.ID))
		}
		for _, c := range m.Clusters {
			attrs := map[string]any{}
			for _, a := range c.Attributes {
				attrs[fmt.Sprintf("0x%04x", a.ID)] = readAttribute(a)
			}
			g.Matter.Attributes[fmt.Sprintf("0x%04x", c.ID)] = attrs
		}
	}
	return g
}

// decodeObject decodes a payload as Instance does.
func decodeObject(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatalf("%s: %v", payload, err)
	}
	return v
}

func setDefault(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// readAttribute is an attribute's value, or why it can't be read.
func readAttribute(a im.Attribute) any {
	var w tlv.Writer
	if err := a.Read(nil, &w, tlv.Anonymous); err != nil {
		return "error: " + err.Error()
	}
	v, err := im.DecodeValue(w.Bytes())
	if err != nil {
		return "error: " + err.Error()
	}
	return v
}
