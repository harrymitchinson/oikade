package ewelinklan

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

const (
	waitLimit = 20 * time.Second
	relayID   = "1000aabbcc"
	probeID   = "1000ddeeff"
)

var root = contractv1.NewRoot("ewelink")

// fakeLAN is the network, as the plugin sees it.
type fakeLAN struct {
	packets chan []byte
	closed  chan struct{}
	once    sync.Once
	queries atomic.Int32
}

func newFakeLAN() *fakeLAN {
	return &fakeLAN{packets: make(chan []byte, 64), closed: make(chan struct{})}
}

func (f *fakeLAN) Read(b []byte) (int, error) {
	select {
	case p := <-f.packets:
		return copy(b, p), nil
	case <-f.closed:
		return 0, errClosedLAN
	}
}

var errClosedLAN = errors.New("closed")

func (f *fakeLAN) Send([]byte) error {
	f.queries.Add(1)
	return nil
}

func (f *fakeLAN) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// fakeDevice speaks the protocol. It answers a command by reporting, as a real one does.
type fakeDevice struct {
	t        *testing.T
	lan      *fakeLAN
	id, kind string
	key      string
	address  netip.AddrPort

	mu sync.Mutex
	// fields is its latest report, and bodies the commands it decrypted.
	fields map[string]any
	bodies []map[string]any
	paths  []string
	// answer is what it says to a command, and silent that it doesn't report the result.
	answer string
	silent bool
}

func (d *fakeDevice) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		d.t.Error(err)
		return
	}
	var body struct {
		DeviceID   string `json:"deviceid"`
		Encrypt    bool   `json:"encrypt"`
		SelfAPIKey string `json:"selfApikey"`
		Sequence   string `json:"sequence"`
		IV         string `json:"iv"`
		Data       string `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		d.t.Error(err)
		return
	}
	if _, err := strconv.ParseInt(body.Sequence, 10, 64); err != nil || body.DeviceID != d.id || !body.Encrypt || body.SelfAPIKey != "123" {
		d.t.Errorf("command envelope: %s", raw)
	}
	iv, _ := base64.StdEncoding.DecodeString(body.IV)
	data, _ := base64.StdEncoding.DecodeString(body.Data)
	command, err := decrypt(iv, data, lanKey(d.key))
	if err != nil {
		d.t.Errorf("decrypting command: %v", err)
		return
	}
	d.mu.Lock()
	d.bodies, d.paths = append(d.bodies, command), append(d.paths, r.URL.Path)
	answer, silent := d.answer, d.silent
	if answer == "" {
		answer = `{"error":0}`
	}
	d.mu.Unlock()
	_, _ = io.WriteString(w, answer)
	if !silent {
		d.applied(command)
		d.report()
	}
}

// applied moves the relay as a command says.
func (d *fakeDevice) applied(command map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.kind == "plug" {
		d.fields = map[string]any{"switches": command["switches"]}
		return
	}
	d.fields["switch"] = command["switch"]
}

// report announces its state under a fresh iv, with its key.
func (d *fakeDevice) report() {
	d.mu.Lock()
	fields := d.fields
	d.mu.Unlock()
	d.announce(d.key, fields)
}

func (d *fakeDevice) announce(key string, fields map[string]any) {
	iv, data, err := encrypt(fields, lanKey(key))
	if err != nil {
		d.t.Fatal(err)
	}
	d.lan.packets <- d.packet(d.txt(iv, data), true)
}

func (d *fakeDevice) txt(iv, data string) []string {
	cut := min(40, len(data))
	return []string{"id=" + d.id, "type=" + d.kind, "encrypt=true", "iv=" + iv, "data1=" + data[:cut], "data2=" + data[cut:]}
}

func name(s string) dnsmessage.Name { return dnsmessage.MustNewName(s + ".") }

func (d *fakeDevice) instance() string {
	// Devices announce themselves in mixed case.
	return "eWeLink_" + d.id + "." + serviceType
}

func (d *fakeDevice) target() string { return "device-" + d.id + ".local" }

// packet is an mDNS response, as a device sends one. An NSEC is there because real responders
// send them, and the parser mustn't choke on a type it doesn't know.
func (d *fakeDevice) packet(txt []string, whole bool) []byte {
	answers := []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: name(d.instance()), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.TXTResource{TXT: txt},
	}}
	var extra []dnsmessage.Resource
	if whole {
		answers = append(answers, d.ptr(), d.srv())
		extra = []dnsmessage.Resource{d.a(), {
			Header: dnsmessage.ResourceHeader{Name: name(d.instance()), Type: dnsmessage.Type(47), Class: dnsmessage.ClassINET, TTL: 120},
			Body:   &dnsmessage.UnknownResource{Type: dnsmessage.Type(47), Data: []byte{0xc0, 0x0c, 0, 1, 0x40}},
		}}
	}
	return mustPack(d.t, dnsmessage.Message{Response: true, Authoritative: true, Answers: answers, Additionals: extra})
}

func (d *fakeDevice) ptr() dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: name(serviceType), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.PTRResource{PTR: name(d.instance())},
	}
}

func (d *fakeDevice) srv() dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: name(d.instance()), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.SRVResource{Port: d.address.Port(), Target: name(d.target())},
	}
}

func (d *fakeDevice) a() dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: name(d.target()), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 120},
		Body:   &dnsmessage.AResource{A: d.address.Addr().As4()},
	}
}

func mustPack(t *testing.T, m dnsmessage.Message) []byte {
	t.Helper()
	raw, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// bench is a running plugin on a fake network and a fake clock.
type bench struct {
	t       *testing.T
	lan     *fakeLAN
	secrets string
	clock   atomic.Int64
	base    time.Time

	mu     sync.Mutex
	topics map[string]string
	// published counts what's published per topic.
	published map[string]int

	handler *bridge
	plugin  *plugin.Plugin
}

// fast keeps the real windows' order, in less time. relayAge stays long, as the fake clock moves it.
var fast = timing{
	settle: 60 * time.Millisecond, requery: 10 * time.Millisecond, sweep: 10 * time.Millisecond, query: time.Hour,
	relayAge: 30 * time.Second, http: 2 * time.Second, confirm: 2 * time.Second,
}

func start(t *testing.T, c Config, timings timing, declared ...string) *bench {
	t.Helper()
	broker := mqtttest.StartBroker(t)
	b := &bench{
		t: t, lan: newFakeLAN(), secrets: t.TempDir(), base: time.Now(),
		topics: map[string]string{}, published: map[string]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())

	listening, stopListening := context.WithCancel(context.Background())
	t.Cleanup(stopListening)
	bus, err := mqtt.Connect(listening, broker, "ewelink-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe(listening, root.Filter(), func(m mqtt.Message) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.topics[m.Topic] = string(m.Payload)
		b.published[m.Topic]++
	}); err != nil {
		t.Fatal(err)
	}

	env := plugin.Environment{MQTTURL: broker, Root: root, SecretsDir: b.secrets}
	devices := make([]contractv1.DeclaredDevice, 0, len(declared))
	for _, id := range declared {
		devices = append(devices, contractv1.DeclaredDevice{ID: id, Name: id, DisplayName: id})
	}
	b.handler = newBridge(c, env, devices, func() (lan, error) { return b.lan, nil }, timings, b.now, slog.New(slog.DiscardHandler))
	started, startCancel := context.WithTimeout(ctx, waitLimit)
	defer startCancel()
	b.plugin, err = plugin.Start(started, plugin.Options{Environment: env, OnSet: b.handler.Set, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := b.handler.Serve(ctx, b.plugin); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = b.plugin.Close(context.Background())
	})
	return b
}

func (b *bench) now() time.Time { return b.base.Add(time.Duration(b.clock.Load())) }

func (b *bench) advance(d time.Duration) { b.clock.Add(int64(d)) }

func (b *bench) secret(id, value string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.secrets, id), []byte(value), 0o600); err != nil {
		b.t.Fatal(err)
	}
}

// device is one that answers commands on a real socket.
func (b *bench) device(id, kind string, fields map[string]any) *fakeDevice {
	b.t.Helper()
	d := &fakeDevice{t: b.t, lan: b.lan, id: id, kind: kind, key: fixtureKey, fields: fields}
	server := httptest.NewServer(d)
	b.t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		b.t.Fatal(err)
	}
	if d.address, err = netip.ParseAddrPort(u.Host); err != nil {
		b.t.Fatal(err)
	}
	return d
}

func (b *bench) topic(name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.topics[name]
}

func (b *bench) eventually(what string, ok func() bool) {
	b.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	b.t.Fatalf("Never saw %s. Topics: %v", what, b.topics)
}

func (b *bench) reports() map[string]contractv1.DeviceReport {
	raw := b.topic(root.Devices())
	out := map[string]contractv1.DeviceReport{}
	if raw == "" {
		return out
	}
	var msg contractv1.Devices
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		b.t.Fatal(err)
	}
	for _, d := range msg.Devices {
		out[d.ID] = d
	}
	return out
}

func (b *bench) waitAvailability(id, want string) {
	b.t.Helper()
	b.eventually(fmt.Sprintf("%s %s", id, want), func() bool { return b.topic(root.Availability(id)) == want })
}

func (b *bench) waitState(id, field, want string) {
	b.t.Helper()
	b.eventually(fmt.Sprintf("%s %s=%s", id, field, want), func() bool {
		var state map[string]any
		if json.Unmarshal([]byte(b.topic(root.State(id))), &state) != nil {
			return false
		}
		return fmt.Sprint(state[field]) == want
	})
}

func (b *bench) relayState() map[string]any {
	var state map[string]any
	_ = json.Unmarshal([]byte(b.topic(root.State(relayID))), &state)
	return state
}

func relayOf(on string) map[string]any {
	return map[string]any{"switches": []any{map[string]any{"outlet": float64(0), "switch": on}}}
}

func TestARelayIsReportedAndCanBeSwitched(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey+"\n") // as echo would leave it
	d := b.device(relayID, "plug", relayOf("off"))
	d.report()

	b.waitAvailability(relayID, contractv1.Online)
	b.waitState(relayID, "on", "false")
	got := b.reports()[relayID]
	if !got.Found || got.Error != "" || strings.Join(got.Capabilities, ",") != "on" || got.DeviceType != "" {
		t.Errorf("got %+v", got)
	}

	if err := b.handler.Set(context.Background(), b.plugin, contractv1.SetCommand{ID: relayID, Capability: "on", Value: true}); err != nil {
		t.Fatal(err)
	}
	b.waitState(relayID, "on", "true")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.paths[0] != "/zeroconf/switches" {
		t.Errorf("posted to %s", d.paths[0])
	}
	want := `map[switches:[map[outlet:0 switch:on]]]`
	if fmt.Sprint(d.bodies[0]) != want {
		t.Errorf("sent %v", d.bodies[0])
	}
}

func TestASensorRelayReportsItsReadings(t *testing.T) {
	b := start(t, Config{Humidity: []string{strings.ToUpper(probeID)}}, fast)
	b.secret(relayID, fixtureKey)
	b.secret(probeID, fixtureKey)
	plain := b.device(relayID, "th_plug", map[string]any{"switch": "on", "currentTemperature": "19.2", "currentHumidity": "55"})
	humid := b.device(probeID, "th_plug", map[string]any{"switch": "off", "currentTemperature": 21.5, "currentHumidity": "60.5"})
	plain.report()
	humid.report()

	b.waitState(relayID, "temperature", "19.2")
	b.waitState(probeID, "humidity", "60.5")
	if caps := strings.Join(b.reports()[relayID].Capabilities, ","); caps != "on,temperature" {
		t.Errorf("no Humidity entry: %s", caps)
	}
	if caps := strings.Join(b.reports()[probeID].Capabilities, ","); caps != "on,temperature,humidity" {
		t.Errorf("with a Humidity entry: %s", caps)
	}
	if _, ok := b.relayState()["humidity"]; ok {
		t.Errorf("humidity reported unannounced: %v", b.relayState())
	}

	if err := b.handler.Set(context.Background(), b.plugin, contractv1.SetCommand{ID: probeID, Capability: "on", Value: true}); err != nil {
		t.Fatal(err)
	}
	humid.mu.Lock()
	defer humid.mu.Unlock()
	if humid.paths[0] != "/zeroconf/switch" {
		t.Errorf("posted to %s", humid.paths[0])
	}
	if want := "map[deviceType:normal mainSwitch:on switch:on]"; fmt.Sprint(humid.bodies[0]) != want {
		t.Errorf("sent %v", humid.bodies[0])
	}
}

func TestAReadingThatIsntOneIsLeftOut(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "th_plug", map[string]any{"switch": "on", "currentTemperature": "unavailable"})
	d.report()
	b.waitAvailability(relayID, contractv1.Online)
	b.waitState(relayID, "on", "true")
	if _, ok := b.relayState()["temperature"]; ok {
		t.Errorf("got %v", b.relayState())
	}
}

func TestADeviceWithNoKeyShowsThatAndComesUpWhenItsAdded(t *testing.T) {
	b := start(t, Config{}, fast)
	d := b.device(relayID, "plug", relayOf("on"))
	d.report()

	b.eventually("the problem", func() bool {
		return b.reports()[relayID].Error == "no LAN key: oikade secret set "+relayID
	})
	b.waitAvailability(relayID, contractv1.Offline)
	if !b.reports()[relayID].Found || strings.Join(b.reports()[relayID].Capabilities, ",") != "on" {
		t.Errorf("A device with no key should be reported as what it is: %+v", b.reports()[relayID])
	}
	err := b.handler.Set(context.Background(), b.plugin, contractv1.SetCommand{ID: relayID, Capability: "on", Value: true})
	if err == nil || !strings.Contains(err.Error(), "no LAN key") {
		t.Errorf("got %v", err)
	}

	// No report follows this: the one already heard is enough.
	b.secret(relayID, fixtureKey)
	b.waitAvailability(relayID, contractv1.Online)
	b.waitState(relayID, "on", "true")
	b.eventually("the problem clearing", func() bool { return b.reports()[relayID].Error == "" })
}

func TestAWrongKeyShowsAsAProblem(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, "not-the-key")
	d := b.device(relayID, "plug", relayOf("on"))
	d.report()

	b.eventually("the problem", func() bool {
		return b.reports()[relayID].Error == "wrong LAN key: oikade secret set "+relayID
	})
	b.waitAvailability(relayID, contractv1.Offline)

	b.secret(relayID, fixtureKey)
	b.waitAvailability(relayID, contractv1.Online)
	b.eventually("the problem clearing", func() bool { return b.reports()[relayID].Error == "" })
}

func TestOneBadReportAmongGoodOnesIsDamage(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "plug", relayOf("on"))
	d.report()
	b.waitAvailability(relayID, contractv1.Online)

	b.mu.Lock()
	lists := b.published[root.Devices()]
	b.mu.Unlock()

	d.announce("another-key", relayOf("on"))
	// A different report after it shows the plugin has taken the bad one in.
	d.announce(fixtureKey, relayOf("off"))
	b.waitState(relayID, "on", "false")
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.published[root.Devices()] != lists {
		t.Errorf("A damaged report changed the device list: %s", b.topics[root.Devices()])
	}
	if got := b.topics[root.Availability(relayID)]; got != contractv1.Online {
		t.Errorf("got %s", got)
	}
}

func TestAKeyThatStopsWorkingShowsOnceReportsAreStale(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "plug", relayOf("on"))
	d.report()
	b.waitAvailability(relayID, contractv1.Online)

	b.advance(fast.relayAge + time.Second)
	d.announce("another-key", relayOf("off"))
	b.eventually("the problem", func() bool {
		return b.reports()[relayID].Error == "wrong LAN key: oikade secret set "+relayID
	})
}

func TestADeviceGoesOfflineWhenItGoesQuietAndComesBack(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "plug", relayOf("on"))
	d.report()
	b.waitAvailability(relayID, contractv1.Online)

	b.advance(fast.relayAge + time.Second)
	b.waitAvailability(relayID, contractv1.Offline)
	if !b.reports()[relayID].Found {
		t.Error("A quiet device should stay reported")
	}

	d.report()
	b.waitAvailability(relayID, contractv1.Online)
}

func TestARepeatedReportIsntNews(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "plug", relayOf("on"))
	iv, data, err := encrypt(relayOf("on"), lanKey(fixtureKey))
	if err != nil {
		t.Fatal(err)
	}
	d.lan.packets <- d.packet(d.txt(iv, data), true)
	b.waitAvailability(relayID, contractv1.Online)

	b.advance(fast.relayAge + time.Second)
	d.lan.packets <- d.packet(d.txt(iv, data), true)
	b.waitAvailability(relayID, contractv1.Offline)
}

func TestTheFirstReportWaitsAndListsDevicesNotHeard(t *testing.T) {
	timings := fast
	timings.settle = 300 * time.Millisecond
	b := start(t, Config{}, timings, probeID)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "plug", relayOf("on"))
	d.report()

	time.Sleep(timings.settle / 3)
	if raw := b.topic(root.Devices()); raw != "" {
		t.Fatalf("reported before it had listened: %s", raw)
	}

	b.waitState(relayID, "on", "true") // what it heard early comes out with the first report
	b.waitAvailability(probeID, contractv1.Offline)
	got := b.reports()
	if !got[relayID].Found || got[probeID].Found {
		t.Errorf("got %+v", got)
	}

	other := b.device(probeID, "plug", relayOf("off"))
	b.secret(probeID, fixtureKey)
	other.report()
	b.waitAvailability(probeID, contractv1.Online)
	b.eventually("it found", func() bool { return b.reports()[probeID].Found })
}

func TestWhatIsntADeviceIsNotReported(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey)
	strange := b.device(probeID, "fireplace", relayOf("on"))
	strange.report()
	diy := b.device("1000111111", "plug", relayOf("on"))
	diy.lan.packets <- diy.packet([]string{"id=" + diy.id, "type=plug", "encrypt=false", "data1=e30="}, true)
	spoofed := b.device("1000222222", "plug", relayOf("on"))
	spoofed.lan.packets <- spoofed.packet([]string{"id=1000333333", "type=plug", "encrypt=true", "iv=" + fixtureIV, "data1=" + vectors[1].data}, true)
	b.lan.packets <- []byte("not dns")
	broken := b.device("1000444444", "plug", relayOf("on"))
	broken.lan.packets <- broken.packet([]string{"id=1000444444", "noequals"}, true)

	d := b.device(relayID, "plug", relayOf("on"))
	d.report()
	b.waitAvailability(relayID, contractv1.Online)
	if got := b.reports(); len(got) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestRecordsCanArriveSeparately(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "plug", relayOf("off"))

	send := func(r ...dnsmessage.Resource) {
		d.lan.packets <- mustPack(t, dnsmessage.Message{Response: true, Answers: r})
	}
	send(d.ptr())
	send(d.srv())
	iv, data, err := encrypt(relayOf("off"), lanKey(fixtureKey))
	if err != nil {
		t.Fatal(err)
	}
	d.lan.packets <- d.packet(d.txt(iv, data), false)
	b.waitAvailability(relayID, contractv1.Online)
	send(d.a())

	b.eventually("the address", func() bool {
		b.handler.mu.Lock()
		defer b.handler.mu.Unlock()
		_, _, known := b.handler.records.address(b.handler.records.services["ewelink_"+relayID+"."+serviceType])
		return known
	})
	if err := b.handler.Set(context.Background(), b.plugin, contractv1.SetCommand{ID: relayID, Capability: "on", Value: true}); err != nil {
		t.Fatal(err)
	}
}

func TestACommandMustBeConfirmed(t *testing.T) {
	timings := fast
	timings.confirm = 50 * time.Millisecond
	b := start(t, Config{}, timings)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "plug", relayOf("off"))
	d.silent = true
	d.report()
	b.waitAvailability(relayID, contractv1.Online)

	err := b.handler.Set(context.Background(), b.plugin, contractv1.SetCommand{ID: relayID, Capability: "on", Value: true})
	if err == nil || !strings.Contains(err.Error(), "didn't report the change") {
		t.Errorf("got %v", err)
	}
}

func TestACommandCanBeRefused(t *testing.T) {
	b := start(t, Config{}, fast)
	b.secret(relayID, fixtureKey)
	d := b.device(relayID, "plug", relayOf("off"))
	d.answer = `{"error":400}`
	d.silent = true
	d.report()
	b.waitAvailability(relayID, contractv1.Online)

	err := b.handler.Set(context.Background(), b.plugin, contractv1.SetCommand{ID: relayID, Capability: "on", Value: true})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("got %v", err)
	}
}

func TestDevicesNotHeardCantBeSet(t *testing.T) {
	b := start(t, Config{}, fast, relayID)
	err := b.handler.Set(context.Background(), b.plugin, contractv1.SetCommand{ID: relayID, Capability: "on", Value: true})
	if err == nil || !strings.Contains(err.Error(), "hasn't been heard") {
		t.Errorf("got %v", err)
	}
}

func TestItAsksForDevices(t *testing.T) {
	timings := fast
	timings.query = 0 // every sweep
	b := start(t, Config{}, timings)
	b.eventually("repeated questions", func() bool { return b.lan.queries.Load() > 3 })
}
