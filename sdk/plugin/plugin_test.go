package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

const root contractv1.Root = "oikade/v1/home/example"

const relayDevices = `{"devices":[{"id":"relay-1","found":true,"manufacturer":"Oikade",` +
	`"model":"Simulated switch","capabilities":["on"]}],"undeclared":0}`

const (
	online  = `{"state":"online","contract":1}`
	offline = `{"state":"offline","contract":1}`
)

var relay = contractv1.DeviceReport{
	ID: "relay-1", Found: true, Manufacturer: "Oikade", Model: "Simulated switch",
	Capabilities: []string{"on"},
}

// logBuffer collects the plugin's JSON log lines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func noSet(context.Context, *Plugin, contractv1.SetCommand) error { return nil }

// start starts a plugin on the broker. It is closed when the test ends.
func start(ctx context.Context, t *testing.T, url string, o Options) (*Plugin, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	o.Environment = Environment{MQTTURL: url, Root: root}
	if o.OnSet == nil {
		o.OnSet = noSet
	}
	o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	p, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p, logs
}

// watcher sees the messages under root, live or retained, in order.
type watcher struct {
	t   *testing.T
	ctx context.Context
	got chan mqtt.Message
}

// watch sees everything under root. To check exact messages, call it before start: the broker
// acks a publish before it passes it on, so a watcher that subscribes straight after can get the
// message twice, retained and live.
func watch(ctx context.Context, t *testing.T, url string) *watcher {
	t.Helper()
	return watchFiltered(ctx, t, url, func(mqtt.Message) bool { return true })
}

// watchLive sees only messages published after it subscribed. Retained ones can arrive at any
// point, even after messages published later by other clients.
func watchLive(ctx context.Context, t *testing.T, url string) *watcher {
	t.Helper()
	return watchFiltered(ctx, t, url, func(m mqtt.Message) bool { return !m.Retained })
}

func watchFiltered(ctx context.Context, t *testing.T, url string, keep func(mqtt.Message) bool) *watcher {
	t.Helper()
	c, err := mqtt.Connect(ctx, url, fmt.Sprintf("watcher-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	w := &watcher{t: t, ctx: ctx, got: make(chan mqtt.Message, 100)}
	if _, err := c.Subscribe(ctx, root.Filter(), func(m mqtt.Message) {
		if keep(m) {
			w.got <- m
		}
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *watcher) next() mqtt.Message {
	w.t.Helper()
	select {
	case m := <-w.got:
		return m
	case <-w.ctx.Done():
		w.t.Fatal("timed out waiting for a message")
		return mqtt.Message{}
	}
}

// expect checks the next messages, as topic=payload.
func (w *watcher) expect(want ...string) {
	w.t.Helper()
	for _, s := range want {
		m := w.next()
		if got := m.Topic + "=" + string(m.Payload); got != s {
			w.t.Fatalf("got %s, want %s", got, s)
		}
	}
}

// sentinel publishes a message the watcher sees after everything published before it.
func (w *watcher) sentinel(url string) string {
	w.t.Helper()
	c, err := mqtt.Connect(w.ctx, url, fmt.Sprintf("sentinel-%d", time.Now().UnixNano()))
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = c.Disconnect(context.Background()) }()
	topic := string(root) + "/sentinel"
	if err := c.Publish(w.ctx, topic, nil, false); err != nil {
		w.t.Fatal(err)
	}
	return topic
}

// quiet checks nothing else has arrived.
func (w *watcher) quiet(url string) {
	w.t.Helper()
	w.expect(w.sentinel(url) + "=")
}

func TestAnnounceOrder(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	w := watch(ctx, t, b.URL)
	p, _ := start(ctx, t, b.URL, Options{})
	w.expect(root.Status() + "=" + online)

	// Until the plugin reports devices, a reconnect leaves the retained devices alone.
	b.Kick(t, p.clientID)
	w.expect(root.Status()+"="+offline, root.Status()+"="+online)
	w.quiet(b.URL)

	if err := p.SetDevices(ctx, []contractv1.DeviceReport{relay, {ID: "bad", Error: "nope"}}, 2); err != nil {
		t.Fatal(err)
	}
	if err := p.SetAvailability(ctx, "relay-1", true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, "relay-1", contractv1.State{"on": true}); err != nil {
		t.Fatal(err)
	}
	devices := root.Devices() + `={"devices":[{"id":"relay-1","found":true,"manufacturer":"Oikade",` +
		`"model":"Simulated switch","capabilities":["on"]},{"id":"bad","found":false,"error":"nope"}],"undeclared":2}`
	announced := []string{devices, root.Availability("relay-1") + "=online", root.State("relay-1") + `={"on":true}`}
	w.expect(announced...)

	b.Kick(t, p.clientID)
	w.expect(append([]string{root.Status() + "=" + offline, root.Status() + "=" + online}, announced...)...)
}

func TestReconnectRepublishes(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	p, logs := start(ctx, t, b.URL, Options{})
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{relay}, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.SetAvailability(ctx, "relay-1", true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, "relay-1", contractv1.State{"on": true}); err != nil {
		t.Fatal(err)
	}

	// A restart loses every retained message. The plugin puts them back by itself.
	b.Restart(t)
	want := map[string]string{
		root.Status():                online,
		root.Devices():               relayDevices,
		root.Availability("relay-1"): "online",
		root.State("relay-1"):        `{"on":true}`,
	}
	for {
		got := map[string]string{}
		for m := range retained(ctx, t, b.URL) {
			got[m.Topic] = string(m.Payload)
		}
		if reflect.DeepEqual(got, want) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("got retained %v, want %v", got, want)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if n := strings.Count(logs.String(), `"msg":"Plugin came online"`); n != 2 {
		t.Fatalf("logged coming online %d times, want 2:\n%s", n, logs)
	}
}

// retained returns the retained messages under root, as a fresh subscriber gets them.
func retained(ctx context.Context, t *testing.T, url string) <-chan mqtt.Message {
	t.Helper()
	w := watch(ctx, t, url)
	out := make(chan mqtt.Message, 100)
	go func() {
		defer close(out)
		for {
			select {
			case m := <-w.got:
				if m.Retained {
					out <- m
				}
			case <-time.After(200 * time.Millisecond):
				return
			}
		}
	}()
	return out
}

func TestCloseWins(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	w := watch(ctx, t, b.URL)
	p, _ := start(ctx, t, b.URL, Options{})
	w.expect(root.Status() + "=" + online)

	// A reconnect's announcement waits behind a publish while Close starts.
	if err := p.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	announced := make(chan struct{})
	go func() {
		p.announce(ctx)
		close(announced)
	}()
	closed := make(chan error, 1)
	go func() { closed <- p.Close(ctx) }()
	for !p.closed.Load() {
		time.Sleep(time.Millisecond)
	}
	p.unlock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	<-announced

	w.expect(root.Status() + "=" + offline)
	w.quiet(b.URL)
	b.WaitGone(t, p.clientID)
	if err := p.SetAvailability(ctx, "relay-1", true); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v after Close, want ErrClosed", err)
	}
}

func TestCloseLeavesItToTheWill(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	w := watch(ctx, t, b.URL)
	p, _ := start(ctx, t, b.URL, Options{CloseTimeout: 50 * time.Millisecond})
	w.expect(root.Status() + "=" + online)

	// A publish that never finishes holds the offline status up.
	if err := p.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(ctx); err == nil {
		t.Fatal("Close succeeded without publishing offline")
	}
	w.expect(root.Status() + "=" + offline) // from the Will
	b.WaitGone(t, p.clientID)
}

func TestStartTimeout(t *testing.T) {
	ctx := testContext(t)
	began := time.Now()
	_, err := Start(ctx, Options{
		Environment:  Environment{MQTTURL: "mqtt://user:secret@127.0.0.1:1", Root: root},
		OnSet:        noSet,
		Logger:       slog.New(slog.DiscardHandler),
		StartTimeout: 200 * time.Millisecond,
	})
	want := "could not come online at mqtt://user:xxxxx@127.0.0.1:1 within 200ms"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want a timeout naming the redacted broker", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Fatalf("took %s to give up", time.Since(began))
	}
}

func TestSetStateValidates(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	// Subscribing before anything is published: mochi races a subscription's retained scan with
	// a concurrent retained publish.
	w := watchLive(ctx, t, b.URL)
	p, _ := start(ctx, t, b.URL, Options{})
	lamp := contractv1.DeviceReport{ID: "lamp", Found: true, Capabilities: []string{"on", "brightness"}}
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{relay, lamp, {ID: "lost"}}, 0); err != nil {
		t.Fatal(err)
	}
	w.expect(root.Status()+"="+online, root.Devices()+`={"devices":[`+
		`{"id":"relay-1","found":true,"manufacturer":"Oikade","model":"Simulated switch","capabilities":["on"]},`+
		`{"id":"lamp","found":true,"capabilities":["on","brightness"]},{"id":"lost","found":false}],"undeclared":0}`)

	for _, tc := range []struct {
		name  string
		id    string
		state contractv1.State
		want  string
	}{
		{"unannounced", "relay-1", contractv1.State{"brightness": 40.0}, "has not announced capability brightness"},
		{"unknown device", "unknown", contractv1.State{"on": true}, "device unknown is not reported found"},
		{"device not found", "lost", contractv1.State{}, "device lost is not reported found"},
		{"out of range", "lamp", contractv1.State{"brightness": 150.0}, "brightness must be between 0 and 100"},
		{"wrong kind", "relay-1", contractv1.State{"on": "on"}, "on takes a bool"},
		{"incomplete", "lamp", contractv1.State{"on": true}, "no value for announced capability brightness"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := p.SetState(ctx, tc.id, tc.state)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if err := p.UpdateState(ctx, tc.id, tc.state); err == nil {
				t.Fatal("UpdateState accepted it")
			}
		})
	}
	if s := p.State("relay-1"); s != nil {
		t.Fatalf("stored %v after refusing it", s)
	}
	if err := p.SetAvailability(ctx, "unknown", true); err == nil {
		t.Fatal("published availability for a device not in SetDevices")
	}

	// Nothing reached the broker: the next state message is a valid one, normalised.
	if err := p.SetState(ctx, "lamp", contractv1.State{"on": true, "brightness": 40}); err != nil {
		t.Fatal(err)
	}
	w.expect(root.State("lamp") + `={"brightness":40,"on":true}`)
	if got := p.State("lamp"); !reflect.DeepEqual(got, contractv1.State{"on": true, "brightness": 40.0}) {
		t.Fatalf("stored %#v, want float64 values", got)
	}
}

func TestUpdateStateMerges(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	p, _ := start(ctx, t, b.URL, Options{})
	lamp := contractv1.DeviceReport{ID: "lamp", Found: true, Capabilities: []string{"on", "brightness"}}
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{lamp}, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, "lamp", contractv1.State{"on": true, "brightness": 40.0}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateState(ctx, "lamp", contractv1.State{"brightness": 80.0}); err != nil {
		t.Fatal(err)
	}
	if got := p.State("lamp"); !reflect.DeepEqual(got, contractv1.State{"on": true, "brightness": 80.0}) {
		t.Fatalf("got %v, want the merged state", got)
	}
}

func TestSetDevicesDropsStale(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	w := watchLive(ctx, t, b.URL) // first, as in TestSetStateValidates
	p, _ := start(ctx, t, b.URL, Options{})
	lamp := contractv1.DeviceReport{ID: "lamp", Found: true, Capabilities: []string{"on", "brightness"}}
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{lamp}, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, "lamp", contractv1.State{"on": true, "brightness": 40.0}); err != nil {
		t.Fatal(err)
	}
	w.expect(
		root.Status()+"="+online,
		root.Devices()+`={"devices":[{"id":"lamp","found":true,"capabilities":["on","brightness"]}],"undeclared":0}`,
		root.State("lamp")+`={"brightness":40,"on":true}`,
	)

	lamp.Capabilities = []string{"on"}
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{lamp}, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{{ID: "lamp"}}, 0); err != nil {
		t.Fatal(err)
	}
	w.expect(
		root.Devices()+`={"devices":[{"id":"lamp","found":true,"capabilities":["on"]}],"undeclared":0}`,
		root.State("lamp")+`={"on":true}`,
		root.Devices()+`={"devices":[{"id":"lamp","found":false}],"undeclared":0}`,
		root.State("lamp")+`={}`,
	)
}

func TestSetDispatch(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	var (
		mu      sync.Mutex
		got     []contractv1.SetCommand
		running atomic.Int32
		overlap atomic.Bool
	)
	onSet := func(_ context.Context, _ *Plugin, cmd contractv1.SetCommand) error {
		if cmd.ID == "relay-1" {
			if running.Add(1) > 1 {
				overlap.Store(true)
			}
			defer running.Add(-1)
			time.Sleep(time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		got = append(got, cmd)
		return nil
	}
	p, logs := start(ctx, t, b.URL, Options{OnSet: onSet})
	lamp := contractv1.DeviceReport{ID: "lamp", Found: true, Capabilities: []string{"brightness"}}
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{relay, lamp}, 0); err != nil {
		t.Fatal(err)
	}

	pub, err := mqtt.Connect(ctx, b.URL, "oikade")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pub.Disconnect(context.Background()) }()
	send := func(topic, payload string) {
		t.Helper()
		if err := pub.Publish(ctx, topic, []byte(payload), false); err != nil {
			t.Fatal(err)
		}
	}
	send(root.Set("relay-1"), `{"brightness":50}`)
	send(root.Set("other"), `{"on":false}`)
	send(root.Set("relay-1"), `not json`)
	send(root.Set("lamp"), `{"brightness":150}`)
	send(root.State("relay-1"), `{"on":false}`)
	want := make([]contractv1.SetCommand, 0, 10)
	for i := range 10 {
		send(root.Set("relay-1"), fmt.Sprintf(`{"on":%v}`, i%2 == 0))
		want = append(want, contractv1.SetCommand{ID: "relay-1", Capability: "on", Value: i%2 == 0})
	}
	send(root.Set("lamp"), `{"brightness":30}`)

	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == len(want)+1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("got %d commands, want %d", n, len(want)+1)
		case <-time.After(10 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	var relayCmds []contractv1.SetCommand
	for _, c := range got {
		if c.ID == "lamp" {
			if c != (contractv1.SetCommand{ID: "lamp", Capability: "brightness", Value: 30.0}) {
				t.Fatalf("got %+v for the lamp", c)
			}
			continue
		}
		relayCmds = append(relayCmds, c)
	}
	if !reflect.DeepEqual(relayCmds, want) {
		t.Fatalf("got %+v, want them in order %+v", relayCmds, want)
	}
	if n := strings.Count(logs.String(), `"msg":"Ignored set command"`); n != 4 {
		t.Fatalf("ignored %d set commands, want 4:\n%s", n, logs)
	}
	if overlap.Load() {
		t.Fatal("one device's commands overlapped")
	}
}

func TestRetainedSetIgnored(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	pub, err := mqtt.Connect(ctx, b.URL, "old-hub")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pub.Disconnect(context.Background()) }()
	if err := pub.Publish(ctx, root.Set("relay-1"), []byte(`{"on":true}`), true); err != nil {
		t.Fatal(err)
	}
	_, logs := start(ctx, t, b.URL, Options{OnSet: func(context.Context, *Plugin, contractv1.SetCommand) error {
		t.Error("applied a retained set command")
		return nil
	}})
	want := `"msg":"Ignored set command","topic":"` + root.Set("relay-1") + `","reason":"retained"`
	for !strings.Contains(logs.String(), want) {
		select {
		case <-ctx.Done():
			t.Fatalf("logged:\n%s\nwant the retained command ignored", logs)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSetHandlerFailure(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	p, logs := start(ctx, t, b.URL, Options{OnSet: func(context.Context, *Plugin, contractv1.SetCommand) error {
		return errors.New("relay unreachable")
	}})
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{relay}, 0); err != nil {
		t.Fatal(err)
	}
	pub, err := mqtt.Connect(ctx, b.URL, "oikade")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pub.Disconnect(context.Background()) }()
	if err := pub.Publish(ctx, root.Set("relay-1"), []byte(`{"on":true}`), false); err != nil {
		t.Fatal(err)
	}
	want := `"msg":"Could not apply set command","topic":"oikade/v1/home/example/relay-1/set",` +
		`"capability":"on","err":"relay unreachable"`
	for !strings.Contains(logs.String(), want) {
		select {
		case <-ctx.Done():
			t.Fatalf("logged:\n%s\nwant %s", logs, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSetDiscovered(t *testing.T) {
	ctx := testContext(t)
	b := mqtttest.NewBroker(t)
	w := watchLive(ctx, t, b.URL) // first, as in TestSetStateValidates
	p, _ := start(ctx, t, b.URL, Options{})
	lamp := contractv1.DiscoveredDevice{
		ID: "lamp-9c71", Model: "LCA001", Capabilities: []string{"on"}, Name: "Desk lamp", Room: "Study",
	}

	for _, bad := range [][]contractv1.DiscoveredDevice{
		{{ID: "a/b"}},
		{lamp, lamp},
		{{ID: "x", Capabilities: []string{"light.sparkle"}}},
	} {
		if err := p.SetDiscovered(ctx, bad); err == nil {
			t.Errorf("SetDiscovered(%+v) succeeded, want an error", bad)
		}
	}
	// An old name is as unknown as a made-up one, and SetDevices refuses it as SetDiscovered does.
	old := []contractv1.DeviceReport{{ID: "relay", Found: true, Capabilities: []string{"switch.on"}}}
	if err := p.SetDevices(ctx, old, 0); err == nil || !strings.Contains(err.Error(), "unknown capability switch.on") {
		t.Errorf("SetDevices with switch.on: %v", err)
	}
	// Before SetDevices, the list waits for it.
	if err := p.SetDiscovered(ctx, []contractv1.DiscoveredDevice{lamp}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{}, 5); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDiscovered(ctx, []contractv1.DiscoveredDevice{}); err != nil {
		t.Fatal(err)
	}
	w.expect(
		root.Status()+"="+online,
		root.Devices()+`={"devices":[],"undeclared":5,"discovered":[{"id":"lamp-9c71","model":"LCA001",`+
			`"capabilities":["on"],"name":"Desk lamp","room":"Study"}]}`,
		root.Devices()+`={"devices":[],"undeclared":5,"discovered":[]}`,
	)

	// A reconnect republishes the list.
	b.Kick(t, p.clientID)
	w.expect(
		root.Status()+"="+offline,
		root.Status()+"="+online,
		root.Devices()+`={"devices":[],"undeclared":5,"discovered":[]}`,
	)

	// nil stops reporting the list.
	if err := p.SetDiscovered(ctx, nil); err != nil {
		t.Fatal(err)
	}
	w.expect(root.Devices() + `={"devices":[],"undeclared":5}`)
}

func TestReportStatePublishesWhatsKnown(t *testing.T) {
	ctx := testContext(t)
	url := mqtttest.StartBroker(t)
	p, _ := start(ctx, t, url, Options{OnSet: func(context.Context, *Plugin, contractv1.SetCommand) error { return nil }})
	sensor := contractv1.DeviceReport{
		ID: "sensor-1", Found: true, Capabilities: []string{"temperature", "battery"},
	}
	if err := p.SetDevices(ctx, []contractv1.DeviceReport{sensor}, 0); err != nil {
		t.Fatal(err)
	}

	if err := p.UpdateState(ctx, "sensor-1", contractv1.State{"temperature": 21.5}); err == nil {
		t.Fatal("UpdateState took a state with a value missing")
	}
	if err := p.ReportState(ctx, "sensor-1", contractv1.State{"temperature": 21.5}); err != nil {
		t.Fatal(err)
	}
	if err := p.ReportState(ctx, "sensor-1", contractv1.State{"battery": 80.0}); err != nil {
		t.Fatal(err)
	}
	want := contractv1.State{"temperature": 21.5, "battery": 80.0}
	if got := p.State("sensor-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if err := p.ReportState(ctx, "sensor-1", contractv1.State{"on": true}); err == nil {
		t.Error("took a capability the device hasn't announced")
	}
	if err := p.ReportState(ctx, "sensor-1", contractv1.State{"battery": 900.0}); err == nil {
		t.Error("took a value out of range")
	}
	if err := p.ReportState(ctx, "nobody", contractv1.State{"battery": 80.0}); err == nil {
		t.Error("took a device that isn't found")
	}
}
