package zigbee2mqtt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/mqtt"
)

// A fixture is one device model as Zigbee2MQTT publishes it: its entry in <base>/bridge/devices,
// and the state and command payloads heard for it. Recorded ones come from a real broker, by
// TestRecord. Golden tests (golden_test.go) replay each one.
//
// Recording, read-only, from the machine your broker serves:
//
//	go test ./plugins/zigbee2mqtt -run TestRecord -timeout 0 -args -record mqtt://broker:1883 -record.for 10m
//
// It subscribes to <base>/# and never publishes. Then run the golden tests with -update, and read
// the fixtures and goldens before committing them.
var (
	recordBroker = flag.String("record", "", "record fixtures from this broker URL, read-only (TestRecord)")
	recordFor    = flag.Duration("record.for", 10*time.Minute, "how long TestRecord listens")
	recordBase   = flag.String("record.base", DefaultBaseTopic, "Zigbee2MQTT's mqtt.base_topic, for TestRecord")
	recordDir    = flag.String("record.dir", fixtureDir, "where TestRecord writes fixtures")
)

const (
	fixtureDir = "testdata/fixtures"
	// fixtureIEEE stands for the device in every fixture, and coordinatorIEEE for the coordinator.
	fixtureIEEE     = "0x0000000000000001"
	coordinatorIEEE = "0x0000000000000000"
	// fixtureLimit bounds the payloads kept for each model.
	fixtureLimit = 50
	// sourceHandWritten marks a fixture nobody recorded, which a recording replaces.
	sourceHandWritten = "hand-written"
)

type fixture struct {
	// Source is sourceHandWritten, or when and from what it was recorded.
	Source string `json:"source"`
	// Device is the model's entry in bridge/devices, with fixtureIEEE for its address and name.
	Device   json.RawMessage   `json:"device"`
	States   []json.RawMessage `json:"states"`
	Commands []json.RawMessage `json:"commands"`
}

func readFixture(path string) (fixture, error) {
	var f fixture
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// encode is a fixture as it's written. It doesn't escape <, > and &, so the leak check sees a name
// as written.
func (f fixture) encode() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(f)
	return b.Bytes(), err
}

// deviceKeys are the fields of a bridge/devices entry a fixture keeps: what describes the model.
// The rest, such as its addresses, bindings and dates, describes the one device.
var deviceKeys = []string{"type", "definition", "manufacturer", "model_id", "power_source", "software_build_id", "supported"}

var (
	ieeePattern = regexp.MustCompile(`0x[0-9a-fA-F]{16}`)
	ipv4Pattern = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
)

// scrubber takes what names devices out of payloads: every friendly name and IEEE address on the
// network, and anything shaped like an address.
type scrubber struct {
	ids map[string]bool
}

func (s scrubber) value(v any) any {
	switch v := v.(type) {
	case string:
		return s.string(v)
	case []any:
		for i := range v {
			v[i] = s.value(v[i])
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[s.string(k)] = s.value(x)
		}
		return out
	}
	return v
}

func (s scrubber) string(v string) string {
	if s.ids[v] {
		return fixtureIEEE
	}
	v = ieeePattern.ReplaceAllString(v, fixtureIEEE)
	return ipv4Pattern.ReplaceAllString(v, "192.0.2.1")
}

// device is a bridge/devices entry as a fixture keeps it.
func (s scrubber) device(entry map[string]any) map[string]any {
	out := map[string]any{"ieee_address": fixtureIEEE, "friendly_name": fixtureIEEE}
	for _, k := range deviceKeys {
		if v, ok := entry[k]; ok {
			out[k] = s.value(v)
		}
	}
	return out
}

// perDevice are the fields Zigbee2MQTT may add to any state payload that describe the one device
// and the moment, not the model: include_device_information's device, and last_seen and elapsed,
// which would also make every payload look new.
var perDevice = []string{"device", "last_seen", "elapsed"}

// state is a state payload as a fixture keeps it.
func (s scrubber) state(payload map[string]any) map[string]any {
	for _, k := range perDevice {
		delete(payload, k)
	}
	return s.value(payload).(map[string]any)
}

// leaks says which of the network's identifiers are still in b.
func (s scrubber) leaks(b []byte) []string {
	var out []string
	for id := range s.ids {
		if len(id) >= 3 && bytes.Contains(b, []byte(id)) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// shape is what makes two payloads different for a fixture: their keys, the type of each value,
// and the value itself for strings and booleans. A temperature of 21.4 and one of 21.5 are the
// same shape; ON and OFF aren't.
func shape(v any) string {
	switch v := v.(type) {
	case map[string]any:
		var b strings.Builder
		b.WriteString("{")
		for _, k := range slices.Sorted(maps.Keys(v)) {
			fmt.Fprintf(&b, "%q:%s,", k, shape(v[k]))
		}
		b.WriteString("}")
		return b.String()
	case []any:
		var b strings.Builder
		b.WriteString("[")
		for _, x := range v {
			b.WriteString(shape(x) + ",")
		}
		b.WriteString("]")
		return b.String()
	case json.Number:
		return "number"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%#v", v)
}

// decodeJSON decodes keeping numbers as written.
func decodeJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

// payloads gathers a model's distinct payloads, by shape.
type payloads struct {
	shapes map[string]bool
	list   []json.RawMessage
}

func (p *payloads) add(v any) error {
	if len(p.list) >= fixtureLimit {
		return nil
	}
	s := shape(v)
	if p.shapes == nil {
		p.shapes = map[string]bool{}
	}
	if p.shapes[s] {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.shapes[s] = true
	p.list = append(p.list, raw)
	return nil
}

// capture is what TestRecord hears.
type capture struct {
	mu      sync.Mutex
	devices []byte
	// names are the IEEE addresses and friendly names in every device list heard, so a device
	// renamed meanwhile is scrubbed by both names.
	names   map[string]bool
	version string
	// stop ends the subscription.
	stop func()
	// messages are the device topics heard (the topic after the base) and their payloads, in order.
	messages []mqtt.Message
}

// captureLimit bounds the device messages a recording keeps.
const captureLimit = 100_000

func (c *capture) hear(base string) mqtt.Handler {
	return func(m mqtt.Message) {
		rest, ok := strings.CutPrefix(m.Topic, base+"/")
		if !ok {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case rest == "bridge/devices":
			c.devices = slices.Clone(m.Payload)
			var list []bridgeDevice
			if json.Unmarshal(m.Payload, &list) == nil {
				if c.names == nil {
					c.names = map[string]bool{}
				}
				for _, d := range list {
					c.names[d.IEEEAddress] = true
					// Zigbee2MQTT's own name for the coordinator names nothing of yours.
					if d.Type != "Coordinator" {
						c.names[d.FriendlyName] = true
					}
				}
			}
		case rest == "bridge/info":
			// Only the version: bridge/info also carries Zigbee2MQTT's configuration, network key included.
			var info struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(m.Payload, &info) == nil {
				c.version = info.Version
			}
		case strings.HasPrefix(rest, "bridge/"):
		case len(c.messages) < captureLimit:
			c.messages = append(c.messages, mqtt.Message{Topic: rest, Payload: slices.Clone(m.Payload)})
		}
	}
}

// modelFixture is one model's fixture as it's being built.
type modelFixture struct {
	device   map[string]any
	states   payloads
	commands payloads
}

// fixtures turns a capture into a fixture for each model, by file name. A model whose fixture still
// names a device is left out, and skipped says which and why.
func (c *capture) fixtures(source string, existing map[string]fixture) (out map[string]fixture, skipped []string, err error) {
	s, byName, models, err := c.models()
	if err != nil {
		return nil, nil, err
	}
	// A recording adds to the one before it.
	for file, m := range models {
		if prev, ok := existing[file]; ok && prev.Source != sourceHandWritten {
			if err := m.merge(prev); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", file, err)
			}
		}
	}
	for _, msg := range c.messages {
		if err := c.take(s, byName, models, msg); err != nil {
			return nil, nil, err
		}
	}
	if c.version != "" {
		source += ", Zigbee2MQTT " + c.version
	}
	out = map[string]fixture{}
	for _, file := range slices.Sorted(maps.Keys(models)) {
		f, err := models[file].fixture(source)
		if err != nil {
			return nil, nil, err
		}
		b, err := f.encode()
		if err != nil {
			return nil, nil, err
		}
		if leaks := s.leaks(b); len(leaks) > 0 {
			skipped = append(skipped, fmt.Sprintf("%s still names a device (%s)", file, strings.Join(leaks, ", ")))
			continue
		}
		out[file] = f
	}
	return out, skipped, nil
}

// models reads the device list: what to scrub, each device's model by its friendly name and IEEE
// address, and a fixture for each model.
func (c *capture) models() (scrubber, map[string]string, map[string]*modelFixture, error) {
	s := scrubber{ids: map[string]bool{}}
	if c.devices == nil {
		return s, nil, nil, errors.New("heard no bridge/devices: is the base topic right, and is Zigbee2MQTT running?")
	}
	var list []map[string]any
	if err := decodeJSON(c.devices, &list); err != nil {
		return s, nil, nil, fmt.Errorf("bridge/devices: %w", err)
	}
	byName := map[string]string{} // friendly name or IEEE address -> file name
	models := map[string]*modelFixture{}
	maps.Copy(s.ids, c.names)
	words := map[string]bool{}
	for _, d := range list {
		ieee, _ := d["ieee_address"].(string)
		name, _ := d["friendly_name"].(string)
		s.ids[ieee] = true
		if d["type"] != "Coordinator" {
			s.ids[name] = true
		}
		def, _ := d["definition"].(map[string]any)
		exposeWords(def["exposes"], words)
		if d["type"] == "Coordinator" || def == nil {
			continue
		}
		vendor, _ := def["vendor"].(string)
		model, _ := def["model"].(string)
		file := fixtureName(vendor, model)
		byName[ieee], byName[name] = file, file
		if models[file] == nil {
			models[file] = &modelFixture{device: s.device(d)}
		}
	}
	delete(s.ids, "")
	// A device named like a property, such as state, names nothing, and replacing it would rewrite
	// every payload.
	for w := range words {
		delete(s.ids, w)
	}
	// The device entries are scrubbed again, now every identifier is known.
	for _, m := range models {
		m.device = s.value(m.device).(map[string]any)
	}
	return s, byName, models, nil
}

// take adds one message to its model's fixture, if it's a device's state or a command to one.
func (c *capture) take(s scrubber, byName map[string]string, models map[string]*modelFixture, msg mqtt.Message) error {
	name, command, ok := deviceTopic(msg.Topic, msg.Payload)
	if !ok {
		return nil
	}
	file, ok := byName[name]
	if !ok {
		return nil
	}
	m := models[file]
	if command != nil {
		return m.commands.add(s.value(command))
	}
	var state map[string]any
	if decodeJSON(msg.Payload, &state) != nil || state == nil {
		return nil
	}
	return m.states.add(s.state(state))
}

// exposeWords adds the name and property of every expose and feature in v to words.
func exposeWords(v any, words map[string]bool) {
	switch v := v.(type) {
	case []any:
		for _, x := range v {
			exposeWords(x, words)
		}
	case map[string]any:
		for _, k := range []string{"name", "property"} {
			if w, ok := v[k].(string); ok {
				words[w] = true
			}
		}
		exposeWords(v["features"], words)
	}
}

func (m *modelFixture) merge(prev fixture) error {
	for _, raw := range prev.States {
		if err := addRaw(&m.states, raw); err != nil {
			return err
		}
	}
	for _, raw := range prev.Commands {
		if err := addRaw(&m.commands, raw); err != nil {
			return err
		}
	}
	return nil
}

func (m *modelFixture) fixture(source string) (fixture, error) {
	device, err := json.Marshal(m.device)
	if err != nil {
		return fixture{}, err
	}
	f := fixture{Source: source, Device: device, States: m.states.list, Commands: m.commands.list}
	if f.States == nil {
		f.States = []json.RawMessage{}
	}
	if f.Commands == nil {
		f.Commands = []json.RawMessage{}
	}
	return f, nil
}

func addRaw(p *payloads, raw json.RawMessage) error {
	var v any
	if err := decodeJSON(raw, &v); err != nil {
		return err
	}
	return p.add(v)
}

// deviceTopic reads a device topic, after the base. A state topic is the device's name. A command
// is <name>/set with an object, or <name>/set/<property> with a value, which it returns as an
// object. Availability and /get aren't either, and ok is false.
func deviceTopic(rest string, payload []byte) (name string, command map[string]any, ok bool) {
	switch {
	case strings.HasSuffix(rest, "/availability"), strings.HasSuffix(rest, "/get"), strings.Contains(rest, "/get/"):
		return "", nil, false
	case strings.HasSuffix(rest, "/set"):
		if decodeJSON(payload, &command) != nil || command == nil {
			return "", nil, false
		}
		return strings.TrimSuffix(rest, "/set"), command, true
	}
	if before, after, ok := strings.CutLast(rest, "/set/"); ok {
		var v any
		if decodeJSON(payload, &v) != nil {
			v = string(payload)
		}
		return before, map[string]any{after: v}, true
	}
	return rest, nil, true
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// fixtureName is a model's file name, such as ikea-led1545g12.json.
func fixtureName(vendor, model string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(vendor+"-"+model), "-"), "-") + ".json"
}

func readFixtures(dir string) (map[string]fixture, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]fixture{}
	for _, p := range paths {
		f, err := readFixture(p)
		if err != nil {
			return nil, err
		}
		out[filepath.Base(p)] = f
	}
	return out, nil
}

// recordConnectTimeout bounds reaching the broker: the MQTT client would otherwise retry forever,
// with nothing said.
const recordConnectTimeout = 10 * time.Second

// TestRecord records fixtures from a real Zigbee2MQTT, read-only. It runs only with -record.
func TestRecord(t *testing.T) {
	if *recordBroker == "" {
		t.Skip("records fixtures only with -record <broker URL>")
	}
	fmt.Fprintf(os.Stderr, "Connecting to %s\n", *recordBroker)
	life, cut := context.WithCancel(context.Background())
	defer cut()
	dial, cancel := context.WithTimeout(life, recordConnectTimeout)
	defer cancel()
	connecting := context.AfterFunc(dial, cut)
	// No will, and record can't publish: the recorder only reads.
	bus, err := mqtt.Connect(life, *recordBroker, "oikade-record-"+clientID())
	if !connecting() {
		t.Fatalf("could not reach %s within %s", *recordBroker, recordConnectTimeout)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bus.Disconnect(context.Background()) }()
	c, err := listen(life, bus, *recordBase)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "Recording %s/# for %s: use your devices now, and send commands from an MQTT client such as oikade set\n",
		*recordBase, *recordFor)
	time.Sleep(*recordFor)
	n, skipped, err := c.write(*recordDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range skipped {
		fmt.Fprintf(os.Stderr, "Skipped %s\n", s)
	}
	fmt.Fprintf(os.Stderr, "Recorded %d models. Run the golden tests with -update, and read both before committing\n", n)
}

// subscriber is all of a bus the recorder may use, so it can't publish.
type subscriber interface {
	Subscribe(ctx context.Context, filter string, h mqtt.Handler) (unsubscribe func(), err error)
}

// listen starts capturing <base>/#.
func listen(ctx context.Context, bus subscriber, base string) (*capture, error) {
	c := &capture{}
	unsubscribe, err := bus.Subscribe(ctx, base+"/#", c.hear(base))
	if err != nil {
		return nil, err
	}
	c.stop = unsubscribe
	return c, nil
}

// write stops capturing, and writes or adds to a fixture in dir for each model. It says how many
// models it wrote, and which it skipped.
func (c *capture) write(dir string) (int, []string, error) {
	if c.stop != nil {
		c.stop()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, err := readFixtures(dir)
	if err != nil {
		return 0, nil, err
	}
	fixtures, skipped, err := c.fixtures("recorded "+time.Now().Format(time.DateOnly), existing)
	if err != nil {
		return 0, nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, nil, err
	}
	for _, file := range slices.Sorted(maps.Keys(fixtures)) {
		f := fixtures[file]
		b, err := f.encode()
		if err != nil {
			return 0, nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
			return 0, nil, err
		}
		fmt.Fprintf(os.Stderr, "Wrote %s: %d states, %d commands\n", file, len(f.States), len(f.Commands))
	}
	return len(fixtures), skipped, nil
}

func TestRecordWritesWhatItHeard(t *testing.T) {
	ctx := context.Background()
	bus := mqtt.NewMemory()
	_ = bus.Publish(ctx, "z2m/bridge/devices", []byte(`[{"ieee_address":"0x00124b00eeeeeeee","friendly_name":"desk","type":"Router",
		"definition":{"vendor":"V","model":"M","exposes":[]}}]`), true)
	_ = bus.Publish(ctx, "z2m/desk", []byte(`{"state":"ON"}`), true)
	c, err := listen(ctx, bus, "z2m")
	if err != nil {
		t.Fatal(err)
	}
	_ = bus.Publish(ctx, "z2m/desk/set", []byte(`{"state":"OFF"}`), false)
	dir := t.TempDir()
	n, skipped, err := c.write(dir)
	if err != nil || n != 1 || skipped != nil {
		t.Fatalf("wrote %d, skipped %v: %v", n, skipped, err)
	}
	// Heard after it stopped: not recorded.
	_ = bus.Publish(ctx, "z2m/desk", []byte(`{"state":"OFF","power":3}`), false)
	f, err := readFixture(filepath.Join(dir, "v-m.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s, c := rawStrings(f.States), rawStrings(f.Commands); !slices.Equal(s, []string{`{"state":"ON"}`}) ||
		!slices.Equal(c, []string{`{"state":"OFF"}`}) {
		t.Errorf("states %s, commands %s", s, c)
	}
}

func TestCaptureKeepsNothingThatNamesADevice(t *testing.T) {
	c := &capture{}
	hear := c.hear("z2m")
	send := func(topic, payload string) { hear(mqtt.Message{Topic: "z2m/" + topic, Payload: []byte(payload)}) }
	send("bridge/info", `{"version":"2.6.0","config":{"advanced":{"network_key":[1,2,3],"pan_id":6754}},"network":{"extended_pan_id":"0xdddddddddddddddd"}}`)
	// Renamed while it was recording: both names are scrubbed.
	send("bridge/devices", `[{"ieee_address":"0x00158dbbbbbbbbbb","friendly_name":"Hall window","type":"EndDevice"}]`)
	send("bridge/devices", `[
	  {"ieee_address":"0x00124b00aaaaaaaa","friendly_name":"Coordinator","type":"Coordinator","network_address":0,"definition":null},
	  {"ieee_address":"0x00158dbbbbbbbbbb","friendly_name":"Kitchen/Window","type":"EndDevice","network_address":12345,
	   "date_code":"20191205","manufacturer":"LUMI","model_id":"lumi.sensor_magnet.aq2","power_source":"Battery",
	   "description":"by the 0x00158dbbbbbbbbbb","endpoints":{"1":{"bindings":[{"target":{"ieee_address":"0x00124b00aaaaaaaa"}}]}},
	   "definition":{"vendor":"Aqara","model":"MCCGQ11LM","exposes":[{"type":"binary","name":"contact","property":"contact","access":1}]}},
	  {"ieee_address":"0x00158dcccccccccc","friendly_name":"0x00158dcccccccccc","type":"EndDevice",
	   "definition":{"vendor":"Aqara","model":"MCCGQ11LM","exposes":[]}}
	]`)
	send("Kitchen/Window", `{"contact":true,"battery":91,"device":{"friendlyName":"Kitchen/Window","ieeeAddr":"0x00158dbbbbbbbbbb"}}`)
	send("Kitchen/Window", `{"contact":true,"battery":90}`) // Same shape: not kept.
	send("0x00158dcccccccccc", `{"contact":false,"battery":90,"last_seen":"2026-11-15T12:00:00Z","elapsed":3,"where":"Kitchen/Window","ip":"192.168.1.20","old":"Hall window"}`)
	send("Kitchen/Window/availability", `{"state":"online"}`)
	send("Kitchen/Window/get", `{"contact":""}`)
	send("Kitchen/Window/set", `{"contact":true}`)
	send("Kitchen/Window/set/contact", `false`)
	send("Elsewhere", `{"contact":true}`) // Not a device.

	got, _, err := c.fixtures("recorded 2026-11-15", nil)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := got["aqara-mccgq11lm.json"]
	if len(got) != 1 || !ok {
		t.Fatalf("fixtures = %v", slices.Collect(maps.Keys(got)))
	}
	b, err := f.encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd", "Kitchen", "Window", "Hall", "2026-11-15T", "elapsed", "192.168", "network", "12345", "20191205", "bindings"} {
		if bytes.Contains(b, []byte(leak)) {
			t.Errorf("fixture has %q:\n%s", leak, b)
		}
	}
	if f.Source != "recorded 2026-11-15, Zigbee2MQTT 2.6.0" {
		t.Errorf("source = %q", f.Source)
	}
	var device map[string]any
	if err := json.Unmarshal(f.Device, &device); err != nil {
		t.Fatal(err)
	}
	if device["ieee_address"] != fixtureIEEE || device["friendly_name"] != fixtureIEEE || device["model_id"] != "lumi.sensor_magnet.aq2" {
		t.Errorf("device = %v", device)
	}
	if want := []string{`{"battery":91,"contact":true}`, `{"battery":90,"contact":false,"ip":"192.0.2.1","old":"0x0000000000000001","where":"0x0000000000000001"}`}; !slices.Equal(rawStrings(f.States), want) {
		t.Errorf("states = %s, want %s", rawStrings(f.States), want)
	}
	if want := []string{`{"contact":true}`, `{"contact":false}`}; !slices.Equal(rawStrings(f.Commands), want) {
		t.Errorf("commands = %s, want %s", rawStrings(f.Commands), want)
	}

	// The next recording adds to it, keeping only new shapes.
	c = &capture{devices: c.devices, messages: []mqtt.Message{
		{Topic: "Kitchen/Window", Payload: []byte(`{"contact":true,"battery":12}`)},
		{Topic: "Kitchen/Window", Payload: []byte(`{"contact":true,"battery":12,"voltage":2900}`)},
	}}
	again, _, err := c.fixtures("recorded 2026-11-16", got)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again["aqara-mccgq11lm.json"].States); n != 3 {
		t.Errorf("after a second recording, %d states, want 3", n)
	}
}

func TestCaptureReplacesAHandWrittenFixture(t *testing.T) {
	c := &capture{
		devices:  []byte(`[{"ieee_address":"0x00158dbbbbbbbbbb","friendly_name":"desk","definition":{"vendor":"V","model":"M","exposes":[]}}]`),
		messages: []mqtt.Message{{Topic: "desk", Payload: []byte(`{"state":"ON"}`)}},
	}
	got, _, err := c.fixtures("recorded", map[string]fixture{
		"v-m.json": {Source: sourceHandWritten, States: []json.RawMessage{json.RawMessage(`{"made_up":1}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := rawStrings(got["v-m.json"].States); !slices.Equal(s, []string{`{"state":"ON"}`}) {
		t.Errorf("states = %s", s)
	}
}

func TestCaptureNeedsTheDeviceList(t *testing.T) {
	if _, _, err := (&capture{}).fixtures("recorded", nil); err == nil || !strings.Contains(err.Error(), "heard no bridge/devices") {
		t.Errorf("a capture with no bridge/devices: got %v, want it to say it heard none", err)
	}
}

func rawStrings(raws []json.RawMessage) []string {
	out := make([]string, 0, len(raws))
	for _, r := range raws {
		var b bytes.Buffer
		if err := json.Compact(&b, r); err != nil {
			panic(err)
		}
		out = append(out, b.String())
	}
	return out
}

func TestCaptureSkipsAModelThatStillNamesADevice(t *testing.T) {
	c := &capture{
		devices: []byte(`[
		  {"ieee_address":"0x00158dbbbbbbbbbb","friendly_name":"bulb","definition":{"vendor":"V","model":"Bulb","description":"A bulb","exposes":[]}},
		  {"ieee_address":"0x00158dcccccccccc","friendly_name":"state","definition":{"vendor":"V","model":"Plug","exposes":[{"type":"binary","name":"state","property":"state"}]}}
		]`),
		messages: []mqtt.Message{{Topic: "state", Payload: []byte(`{"state":"ON"}`)}},
	}
	got, skipped, err := c.fixtures("recorded", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The bulb's own description has its name in it, so it's left out, and the plug written.
	if len(skipped) != 1 || !strings.Contains(skipped[0], "v-bulb.json") {
		t.Errorf("skipped = %v", skipped)
	}
	// A device named like a property doesn't rewrite the property.
	if s := rawStrings(got["v-plug.json"].States); !slices.Equal(s, []string{`{"state":"ON"}`}) {
		t.Errorf("plug states = %s", s)
	}
}
