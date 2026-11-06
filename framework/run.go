package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

// dialTimeout bounds opening a session with the host.
const dialTimeout = 10 * time.Second

var hostInstance = driver.InstanceKey{Driver: "host"}

func run(h Home, log logr.Logger) error {
	rules, flows, err := h.compile()
	if err != nil {
		return err
	}
	if len(os.Args) > 1 && os.Args[1] == wire.RoleCheck {
		return json.NewEncoder(os.Stdout).Encode(h.manifest())
	}
	if len(os.Args) < 2 || (os.Args[1] != wire.RoleAutomations && os.Args[1] != wire.RolePlugin) {
		return errors.New("this is a home: run it with oikade host")
	}
	socket := os.Getenv(wire.SocketEnv)
	if socket == "" {
		return fmt.Errorf("%s isn't set: run this with oikade host", wire.SocketEnv)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == wire.RolePlugin {
		return runPlugin(ctx, h, socket)
	}

	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Key.Name)
	}
	dial, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	client, welcome, err := wire.Dial(dial, socket, names, h.manifest().Rules...)
	if err != nil {
		return err
	}
	defer client.Close()

	// The mirror is the host's device state, here so that conditions never wait for a socket. It's
	// filled before the engine listens, so a Welcome doesn't look like a change to anything.
	mirror := hub.NewStore(logr.Discard())
	metas := map[types.NamespacedName]hub.Meta{}
	for _, d := range welcome.Devices {
		metas[deviceKey(d.Name)] = meta(d)
	}
	mirror.SetDevices(hostInstance, metas)
	for _, d := range welcome.Devices {
		seed(mirror, d)
	}
	eng := &engine.Engine{
		Store: mirror, Setter: driver.SetterFunc(client.Set), States: client, Arbitration: client,
		Log: log.WithName("automations"),
	}
	gather := observe(logr.NewContext(ctx, log), eng, client)
	fires := make(chan timerFire, firesWaiting)
	eng.OnSchedule = func(key types.NamespacedName, s engine.Schedule) {
		if s.LastFire.IsZero() {
			return
		}
		// A fire not saved is caught up once more after a crash, which actions can stand.
		select {
		case fires <- timerFire{key.Name, s.LastFire}:
		default:
		}
	}
	eng.Init()
	mirror.Listeners = []driver.Sink{eng}
	paused := pausedRules(eng, client, rules, welcome.Paused)
	var unaccounted []string
	for _, r := range rules {
		if paused.has(r.Key.Name) {
			continue
		}
		if r.HasTimers() {
			since, ok := welcome.Fires[r.Key.Name]
			if !ok {
				since = eng.Clock.Now()
				unaccounted = append(unaccounted, r.Key.Name)
			}
			r.KeepSince(since)
		}
		eng.Load(r)
	}
	for _, f := range flows {
		f.Journal, f.Actions, f.Clock, f.Log, f.Process = client, eng, eng.Clock, log.WithName("flows"), ctx
	}

	pings := make(chan uint64, 1)
	go answerPings(ctx, client, eng, pings)
	go client.Listen(wire.Events{
		Changed:    func(m wire.Message) { changed(mirror, m) },
		Discovered: func(d wire.Device) { discovered(mirror, metas, d) },
		Ping: func(id uint64) {
			select {
			case pings <- id:
			default:
			}
		},
		Paused: paused.change,
	})
	go paused.run(ctx)
	go gather()

	// The first start has missed nothing, and the times of day count from here.
	now := eng.Clock.Now()
	for _, name := range unaccounted {
		if err := client.SaveFire(ctx, name, now); err != nil {
			return fmt.Errorf("saving a timer: %w", err)
		}
	}
	go saveFires(logr.NewContext(ctx, log), client, fires)
	flowCtx := logr.NewContext(ctx, log.WithName("flows"))
	// The host drops a paused flow's runs. This is for a pause that landed since.
	bodies := flow.Recover(flowCtx, wire.Namespace, paused.without(flows), savedRuns(welcome.Unfinished), client, eng)
	log.Info("Loaded rules", "rules", len(rules), "devices", len(welcome.Devices), "resumedRuns", len(bodies))
	for name, body := range bodies {
		eng.Resume(ctx, deviceKey(name), body)
	}

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	select {
	case <-client.Done():
		// A process cut off from the host can't act. A stuck condition would hold up a clean stop,
		// so this doesn't wait for the engine: nothing here needs saving.
		if err := client.Err(); err != nil {
			return fmt.Errorf("lost the host: %w", err)
		}
		return errors.New("lost the host")
	case <-ctx.Done():
		<-done
		return nil
	}
}

// runPlugin is the process for one of the home's plugins.
func runPlugin(ctx context.Context, h Home, socket string) error {
	if len(os.Args) != 3 {
		return errors.New("which plugin? run this with oikade host")
	}
	i := slices.IndexFunc(h.Plugins, func(p Plugin) bool { return p.Name == os.Args[2] })
	if i < 0 {
		return fmt.Errorf("this home has no plugin %s", os.Args[2])
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	return plugin.RunHosted(ctx, socket, h.Plugins[i].Name, h.Plugins[i].New, log)
}

// firesWaiting bounds the fires waiting to be saved.
const firesWaiting = 64

// timerFire is a rule's time of day having been accounted for.
type timerFire struct {
	rule string
	at   time.Time
}

// saveFires tells the host about fires, apart from the engine, which can't wait for a socket.
func saveFires(ctx context.Context, client *wire.Client, fires <-chan timerFire) {
	log := logr.FromContextOrDiscard(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-client.Done():
			return
		case f := <-fires:
			if err := client.SaveFire(ctx, f.rule, f.at); err != nil {
				log.Error(err, "Failed to save timer fire", "rule", f.rule)
			}
		}
	}
}

func savedRuns(runs []wire.Run) []flow.Saved {
	saved := make([]flow.Saved, 0, len(runs))
	for _, r := range runs {
		saved = append(saved, flow.Saved{ID: r.ID, Entries: r.Entries})
	}
	return saved
}

// answerPings answers once the engine is free, so a stuck condition stops the answers and the host
// replaces the process. A goroutine of its own keeps that from holding up messages from the host.
func answerPings(ctx context.Context, client *wire.Client, eng *engine.Engine, pings <-chan uint64) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-client.Done():
			return
		case id := <-pings:
			eng.Responsive()
			client.Pong(id)
		}
	}
}

func deviceKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: wire.Namespace, Name: name}
}

func meta(d wire.Device) hub.Meta {
	m := hub.Meta{DisplayName: d.Name, Room: d.Room, Instance: hostInstance}
	for _, c := range d.Arbitrated {
		// Only the name matters here. The host holds the settings, and runs the arbiter.
		m.Control = append(m.Control, homev1alpha1.CapabilityControl{Capability: c})
	}
	return m
}

func seed(mirror *hub.Store, d wire.Device) {
	key := deviceKey(d.Name)
	for name, v := range d.Values {
		mirror.Changed(driver.Update{Device: key, Capability: name, Value: v})
	}
	if d.Discovered {
		mirror.Discovered(key, driver.Info{Found: true, Capabilities: d.Capabilities})
	}
}

// discovered brings a device up to date, adding it if it's new since the Welcome. metas are the
// mirror's devices, which it replaces as a set. Only the read goroutine calls this, after the
// Welcome's seeding.
func discovered(mirror *hub.Store, metas map[types.NamespacedName]hub.Meta, d wire.Device) {
	metas[deviceKey(d.Name)] = meta(d)
	mirror.SetDevices(hostInstance, metas)
	for name, v := range d.Values {
		changed(mirror, wire.Message{Device: d.Name, Capability: name, Value: v})
	}
	mirror.Discovered(deviceKey(d.Name), driver.Info{Found: true, Capabilities: d.Capabilities})
}

// changed applies a change. The host may repeat one around the Welcome, so a value it already has
// isn't one: drivers only report actual changes.
func changed(mirror *hub.Store, m wire.Message) {
	key := deviceKey(m.Device)
	if snap, ok := mirror.Get(key); ok {
		if v, known := snap.Values[m.Capability]; known && v == m.Value {
			return
		}
	}
	mirror.Changed(driver.Update{
		Device: key, Capability: m.Capability, Value: m.Value, Previous: m.Previous, Time: m.Time,
	})
}
