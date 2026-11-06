// Package conformance drives a plugin through every topic of the oikade/v1 contract
// (docs/plugin-contract.md) with a fake hub: an in-process broker and Oikade's own decoder.
//
// The checks run under go test (test/conformance) or the oikade-conformance command
// (cmd/oikade-conformance), so they report through T, which *testing.T satisfies.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/driver/plugin"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/mqtt"
	"github.com/harrymitchinson/oikade/internal/mqtt/mqtttest"
)

const (
	namespace = "conformance"
	name      = "plugin"
	// DefaultTimeout bounds every step unless Config says otherwise: a plugin has this long to
	// react.
	DefaultTimeout = 15 * time.Second
)

var root = contractv1.NewRoot(namespace, name)

// T is what the checks report through. *testing.T satisfies it.
type T interface {
	Helper()
	Cleanup(func())
	Error(args ...any)
	Errorf(format string, args ...any)
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Log(args ...any)
	Logf(format string, args ...any)
	Failed() bool
}

// Config says which plugin to check and how.
type Config struct {
	// Command starts the plugin. It is run without a shell, in Dir.
	Command []string
	// Dir is the plugin's working directory. Empty means the current one.
	Dir string
	// Devices is the devices.json handed to the plugin. It must declare at least one device the
	// plugin can find without real hardware or credentials.
	Devices string
	// Secrets is a directory passed as OIKADE_SECRETS_DIR. Optional.
	Secrets string
	// Plugin, if set, checks a plugin a home imports, through the Go API rather than MQTT: Command
	// is the home's binary, and Plugin the plugin's name in it. Only what carries the messages
	// differs, so the checks are the same.
	Plugin string
	// Timeout bounds every step. Zero means DefaultTimeout.
	Timeout time.Duration
	// Log receives the decoder's logs. The zero value discards them.
	Log logr.Logger
}

// Check is one named part of the suite. Checks run in order, against the same plugin process,
// and each depends on the ones before it.
type Check struct {
	Name string
	Run  func(T)
}

// Suite is the fake hub: a broker, Oikade's decoder for the plugin, and the plugin process.
type Suite struct {
	ctx     context.Context
	url     string
	dir     string
	command []string
	env     []string
	wait    time.Duration
	inst    *plugin.Instance
	sink    *recorder
	devices map[string]types.NamespacedName // id -> Device
	p       *process
	// t is the top-level test: plugin processes outlive the check that starts them.
	t T

	mu     sync.Mutex
	report map[string]bool // id -> found, from the raw devices topic
	// online is when each id last published availability online live, not replayed from retain.
	online map[string]time.Time
	// liveDiscovered is the discovered list of the last devices report published live, not
	// replayed from retain, and nil if it had none.
	liveDiscovered map[string]bool
	liveReportAt   time.Time
}

// New starts the fake hub and the plugin for cfg, reporting setup failures and cleaning up
// through t. Run its Checks in order, then t's cleanups stop everything.
func New(t T, cfg Config) *Suite {
	t.Helper()
	if len(cfg.Command) == 0 {
		t.Fatal("no plugin command")
	}
	wait := cfg.Timeout
	if wait <= 0 {
		wait = DefaultTimeout
	}
	broker, err := mqtttest.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Suite{
		ctx: ctx, url: broker.URL, dir: cfg.Dir, command: cfg.Command, wait: wait,
		sink: newRecorder(), devices: readDeclared(t, cfg.Devices), t: t,
	}
	bus, err := mqtt.Connect(ctx, s.url, "conformance-hub")
	if err != nil {
		t.Fatal(err)
	}
	s.inst = &plugin.Instance{
		Key:  types.NamespacedName{Namespace: namespace, Name: name},
		Root: root, Bus: bus, Sink: s.sink, Now: time.Now, Log: cfg.Log,
	}
	if err := s.inst.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.inst.Stop)
	s.inst.SetDevices(s.devices)
	if _, err := bus.Subscribe(ctx, root.Devices(), s.recordReport); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe(ctx, string(root)+"/+/availability", s.recordAvailability); err != nil {
		t.Fatal(err)
	}

	s.env = append(os.Environ(),
		contractv1.EnvMQTTURL+"="+s.url,
		contractv1.EnvTopic+"="+string(root),
		contractv1.EnvDevicesFile+"="+cfg.Devices,
	)
	if cfg.Plugin != "" {
		socket := fakeHost(ctx, t, cfg, bus)
		s.command = append(slices.Clone(cfg.Command), wire.RolePlugin, cfg.Plugin)
		s.env = append(os.Environ(), wire.SocketEnv+"="+socket)
	}
	if cfg.Secrets != "" {
		s.env = append(s.env, contractv1.EnvSecretsDir+"="+cfg.Secrets)
	}
	s.p = s.run()
	return s
}

// Checks returns the suite's checks in the order they must run.
func (s *Suite) Checks() []Check {
	return []Check{
		{"announces itself and every declared device", s.announces},
		{"retains what it publishes and nothing on set", s.retains},
		{"applies set commands and reports the result", s.appliesSet},
		{"goes offline on SIGTERM", s.stops},
		{"comes back after a restart", s.restarts},
		{"leaves a Last Will when killed", s.crashes},
	}
}

// readDeclared returns the devices file's ids, each mapped to a Device the decoder reports for.
func readDeclared(t T, file string) map[string]types.NamespacedName {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var declared contractv1.DevicesFile
	err = json.Unmarshal(raw, &declared)
	if err != nil || declared.Contract != contractv1.Contract || len(declared.Devices) == 0 {
		t.Fatalf("%s must be a contract %d devices file with at least one device: %v", file, contractv1.Contract, err)
	}
	devices := map[string]types.NamespacedName{}
	for _, d := range declared.Devices {
		devices[d.ID] = types.NamespacedName{Namespace: namespace, Name: "device-" + d.ID}
	}
	return devices
}

func (s *Suite) run() *process { return start(s.t, s.dir, s.command, s.env) }

// recordReport keeps the raw devices report, to tell the plugin's own report from the decoder's
// initial "not found".
func (s *Suite) recordReport(m mqtt.Message) {
	var msg contractv1.Devices
	if json.Unmarshal(m.Payload, &msg) != nil {
		return
	}
	r := map[string]bool{}
	for _, d := range msg.Devices {
		r[d.ID] = d.Found
	}
	var discovered map[string]bool
	if msg.Discovered != nil {
		discovered = map[string]bool{}
		for _, d := range msg.Discovered {
			discovered[d.ID] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report = r
	if !m.Retained {
		s.liveDiscovered, s.liveReportAt = discovered, time.Now()
	}
}

func (s *Suite) recordAvailability(m mqtt.Message) {
	id, ok := strings.CutSuffix(strings.TrimPrefix(m.Topic, string(root)+"/"), "/availability")
	if !ok || m.Retained || string(m.Payload) != contractv1.Online {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.online == nil {
		s.online = map[string]time.Time{}
	}
	s.online[id] = time.Now()
}

// reported is true once the plugin's devices report lists every declared device and the decoder
// has caught up with it.
func (s *Suite) reported() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, dev := range s.devices {
		found, listed := s.report[id]
		if !listed || s.sink.info(dev).Found != found {
			return false
		}
	}
	return s.report != nil
}

func (s *Suite) noRejections(t T) {
	t.Helper()
	if r := s.inst.Stats().Rejected; len(r) > 0 {
		t.Errorf("Oikade rejected messages from the plugin: %v", r)
	}
}

func (s *Suite) up() bool { return s.inst.Stats().Up }

func (s *Suite) announces(t T) {
	s.eventually(t, "status online with contract 1", s.up)
	s.eventually(t, "a devices report listing every declared device", s.reported)
	for id, dev := range s.devices {
		info := s.sink.info(dev)
		if !info.Found {
			if info.Error == "" {
				t.Logf("device %s is not found (no error reported)", id)
			}
			continue
		}
		if len(info.Capabilities) == 0 {
			t.Errorf("device %s is found but announces no capabilities Oikade knows", id)
		}
		s.eventually(t, "availability for "+id, func() bool { _, ok := s.sink.online(dev); return ok })
		s.eventually(t, "state for "+id, func() bool { return s.sink.seenAt(dev) })
		for _, c := range info.Capabilities {
			s.eventually(t, fmt.Sprintf("a value of %s for %s", c, id), func() bool {
				_, ok := s.sink.value(dev, c)
				return ok
			})
		}
	}
	if len(s.sink.foundDevices()) == 0 {
		t.Error("the plugin found none of the declared devices; the suite needs at least one")
	}
	s.noRejections(t)
}

func (s *Suite) retains(t T) {
	retained := snapshot(t, s.url)
	if st := retained[root.Status()]; !isStatus(st, contractv1.Online) {
		t.Errorf("status not retained as online: %s", st)
	}
	if retained[root.Devices()] == nil {
		t.Error("devices not retained")
	}
	for id, dev := range s.devices {
		if !s.sink.info(dev).Found {
			continue
		}
		for _, topic := range []string{root.State(id), root.Availability(id)} {
			if retained[topic] == nil {
				t.Errorf("%s not retained", topic)
			}
		}
	}
	for topic := range retained {
		if strings.HasSuffix(topic, "/set") {
			t.Errorf("%s is retained; set commands must never be", topic)
		}
	}
}

func (s *Suite) appliesSet(t T) {
	var tried int
	for id, dev := range s.devices {
		for _, c := range s.sink.info(dev).Capabilities {
			spec, _ := capability.Lookup(c)
			if !spec.Writable {
				continue
			}
			current, _ := s.sink.value(dev, c)
			next := toggled(spec, current)
			if err := s.inst.Set(s.ctx, dev, c, next); err != nil {
				t.Errorf("set %s on %s: %v", c, id, err)
				continue
			}
			tried++
			s.eventually(t, fmt.Sprintf("%s on %s to report %v", c, id, next), func() bool {
				v, _ := s.sink.value(dev, c)
				return v == next
			})
		}
	}
	if tried == 0 {
		t.Log("no found device announces a writable capability; set was not exercised")
	}
	s.noRejections(t)
}

func (s *Suite) stops(t T) {
	s.p.signal(t, syscall.SIGTERM)
	select {
	case <-s.p.done:
		if code := s.p.cmd.ProcessState.ExitCode(); code != 0 {
			t.Errorf("plugin exited with %d after SIGTERM, want 0", code)
		}
	case <-time.After(s.wait):
		t.Fatalf("plugin did not exit within %s of SIGTERM", s.wait)
	}
	s.eventually(t, "status offline", func() bool { return !s.up() })
	for _, dev := range s.sink.foundDevices() {
		s.eventually(t, dev.Name+" unavailable", func() bool { on, _ := s.sink.online(dev); return !on })
	}
}

func (s *Suite) restarts(t T) {
	before, discovers := s.inst.Discovered()
	since := time.Now()
	s.p = s.run()
	s.eventually(t, "status online", s.up)
	if discovers {
		s.keepsDiscoveredIDs(t, before, since)
	}
	// The decoder replays the availability retained before the restart, so only a fresh message
	// shows the plugin republishes it after a Last Will.
	for id, dev := range s.devices {
		if !s.sink.info(dev).Found {
			continue
		}
		s.eventually(t, id+" to republish availability online", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.online[id].After(since)
		})
		s.eventually(t, id+" available again", func() bool { on, _ := s.sink.online(dev); return on })
	}
	s.noRejections(t)
}

// keepsDiscoveredIDs checks that a devices report published after since discovers every device
// discovered before it, by the same ID: Oikade's Devices for adopted devices refer to them by it.
func (s *Suite) keepsDiscoveredIDs(t T, before []contractv1.DiscoveredDevice, since time.Time) {
	t.Helper()
	missing := func() []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		var m []string
		for _, d := range before {
			if !s.liveReportAt.After(since) || !s.liveDiscovered[d.ID] {
				m = append(m, d.ID)
			}
		}
		return m
	}
	deadline := time.Now().Add(s.wait)
	for m := missing(); len(m) > 0; m = missing() {
		if time.Now().After(deadline) {
			t.Errorf("after a restart, the plugin no longer discovers %s within %s: "+
				"a discovered device must keep its ID until it is reset or re-paired", strings.Join(m, ", "), s.wait)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *Suite) crashes(t T) {
	s.p.signal(t, syscall.SIGKILL)
	<-s.p.done
	s.eventually(t, "the Last Will to mark the plugin offline", func() bool { return !s.up() })
	st := snapshot(t, s.url)[root.Status()]
	if !isStatus(st, contractv1.Offline) {
		t.Errorf("retained status after a crash = %s, want offline with contract %d", st, contractv1.Contract)
	}
	s.noRejections(t)
}

// isStatus reports whether payload is a status with this state and the contract version.
func isStatus(payload []byte, state string) bool {
	var s contractv1.Status
	return json.Unmarshal(payload, &s) == nil && s == contractv1.Status{State: state, Contract: contractv1.Contract}
}

// toggled returns a different valid value for a writable capability.
func toggled(spec capability.Spec, current any) any {
	if spec.Kind == homev1alpha1.CapabilityKindBool {
		b, _ := current.(bool)
		return !b
	}
	mid := float64(int((spec.Min + spec.Max) / 2))
	if current == mid {
		return spec.Min
	}
	return mid
}

func (s *Suite) eventually(t T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(s.wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", s.wait, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// snapshot returns the retained messages under the plugin's root, as a new subscriber sees them.
func snapshot(t T, url string) map[string][]byte {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := mqtt.Connect(ctx, url, fmt.Sprintf("conformance-snapshot-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	got := map[string][]byte{}
	unsubscribe, err := c.Subscribe(ctx, root.Filter(), func(m mqtt.Message) {
		if !m.Retained {
			return
		}
		mu.Lock()
		got[m.Topic] = bytes.Clone(m.Payload)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	// Retained messages arrive straight after SUBACK; give them a moment.
	time.Sleep(time.Second)
	unsubscribe()
	mu.Lock()
	defer mu.Unlock()
	return maps.Clone(got)
}

type process struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func start(t T, dir string, command, env []string) *process {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir, cmd.Env = dir, env
	// A child the plugin leaves behind, such as go run's, can hold the output pipe open after
	// the plugin exits. Stop waiting for it rather than hang.
	cmd.WaitDelay = time.Second
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting plugin: %v", err)
	}
	p := &process{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			t.Logf("plugin output:\n%s", out.String())
		}
	})
	return p
}

func (p *process) signal(t T, sig os.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signalling plugin: %v", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// recorder is a driver.Sink that keeps the latest of everything.
type recorder struct {
	mu        sync.Mutex
	infos     map[types.NamespacedName]driver.Info
	available map[types.NamespacedName]bool
	values    map[types.NamespacedName]map[string]any
	seen      map[types.NamespacedName]bool
}

func newRecorder() *recorder {
	return &recorder{
		infos:     map[types.NamespacedName]driver.Info{},
		available: map[types.NamespacedName]bool{},
		values:    map[types.NamespacedName]map[string]any{},
		seen:      map[types.NamespacedName]bool{},
	}
}

func (r *recorder) Discovered(dev types.NamespacedName, info driver.Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.infos[dev] = info
}

func (r *recorder) Changed(u driver.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values[u.Device] == nil {
		r.values[u.Device] = map[string]any{}
	}
	r.values[u.Device][u.Capability] = u.Value
}

func (r *recorder) Availability(dev types.NamespacedName, online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.available[dev] = online
}

func (r *recorder) Seen(dev types.NamespacedName, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[dev] = true
}

func (r *recorder) Undeclared(driver.InstanceKey, int)               {}
func (r *recorder) Inventory(driver.InstanceKey, []driver.AppDevice) {}

func (r *recorder) info(dev types.NamespacedName) driver.Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.infos[dev]
}

func (r *recorder) foundDevices() []types.NamespacedName {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []types.NamespacedName
	for d, i := range r.infos {
		if i.Found {
			out = append(out, d)
		}
	}
	return out
}

func (r *recorder) online(dev types.NamespacedName) (online, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	online, ok = r.available[dev]
	return online, ok
}

func (r *recorder) seenAt(dev types.NamespacedName) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[dev]
}

func (r *recorder) value(dev types.NamespacedName, c string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.values[dev][c]
	return v, ok
}
