package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/flow"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/simhome"
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
	if len(os.Args) > 1 && os.Args[1] == wire.RoleDev {
		return runDev(h)
	}
	if len(os.Args) > 1 && os.Args[1] == wire.RoleReplay {
		return runReplay(h)
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
	return runAutomations(logr.NewContext(ctx, log), h, rules, flows, socket)
}

// runAutomations is the one process that runs a home's rules.
func runAutomations(ctx context.Context, h Home, rules []*engine.Compiled, flows map[string]*flow.Flow,
	socket string,
) error {
	log := logr.FromContextOrDiscard(ctx)
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Key.Name)
	}
	dial, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	settings, err := h.settings()
	if err != nil {
		return err
	}
	client, welcome, err := wire.Dial(dial, socket, names, settings, h.manifest().Rules...)
	if err != nil {
		return err
	}
	defer client.Close()

	// The mirror is the host's device state, here so that conditions never wait for a socket. It's
	// filled before the engine listens, so a Welcome doesn't look like a change to anything.
	mirror := hub.NewStore(logr.Discard())
	metas := map[ident.ID]hub.Meta{}
	for _, d := range welcome.Devices {
		metas[deviceKey(d.Name)] = meta(d, settings)
	}
	mirror.SetDevices(hostInstance, metas)
	for _, d := range welcome.Devices {
		seed(mirror, d)
	}
	eng := &engine.Engine{
		Store: mirror, Setter: driver.SetterFunc(client.Set), States: client, Arbitration: client,
		Log: log.WithName("automations"),
		// A budget has no Rule to show this on, so the log is where you find it.
		OnBudget: func(key ident.ID, r engine.BudgetReport) {
			if r.Unplanned {
				log.Info("Could not plan budget", "rule", key.Name, "reason", r.Reason, "message", r.Message)
			}
		},
	}
	gather := observe(logr.NewContext(ctx, log), eng, client)
	fires := make(chan timerFire, firesWaiting)
	eng.OnSchedule = func(key ident.ID, s engine.Schedule) {
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
		Discovered: func(d wire.Device) { discovered(mirror, metas, settings, d) },
		Seen:       func(m wire.Message) { mirror.Seen(deviceKey(m.Device), m.Time) },
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

// runDev is oikade dev: the home on a simulated one, driven from standard input. The devices are the
// ones in the file oikade types wrote, named after the role.
func runDev(h Home) error {
	if len(os.Args) != 3 {
		return errors.New("run this with oikade dev")
	}
	devices, err := simhome.ReadTypes(os.Args[2])
	if err != nil {
		return err
	}
	home, err := h.simulated()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Standard output is the conversation with you, so the log goes beside it.
	log := funcr.New(func(prefix, args string) { fmt.Fprintln(os.Stderr, prefix, args) }, funcr.Options{})
	return simhome.Dev(ctx, simhome.Options{
		Home: home, Devices: devices, Start: time.Now().Truncate(time.Minute), Location: time.Local, Log: log,
	}, os.Stdin, os.Stdout)
}

// runReplay is oikade replay's half that needs your code: the home against an event log, with no
// socket, no plugin and no device. The role is followed by the log, the types file and where the
// report goes: a file, because your code is free to print.
func runReplay(h Home) error {
	if len(os.Args) != 5 {
		return errors.New("run this with oikade replay")
	}
	devices, err := simhome.ReadTypes(os.Args[3])
	if err != nil {
		return err
	}
	home, err := h.simulated()
	if err != nil {
		return err
	}
	// Errors only: a mistake in a condition is what explains a missing write, and a week of the rest
	// would bury it.
	log := funcr.New(func(prefix, args string) { fmt.Fprintln(os.Stderr, prefix, args) }, funcr.Options{Verbosity: -1})
	report, err := simhome.ReplayFile(simhome.Options{Home: home, Devices: devices, Log: log}, os.Args[2])
	if err != nil {
		return err
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return os.WriteFile(os.Args[4], raw, 0o600)
}

// simulated is the home for a simulated one to run.
func (h Home) simulated() (simhome.Home, error) {
	settings, err := h.settings()
	if err != nil {
		return simhome.Home{}, err
	}
	return simhome.Home{Compile: h.compile, Control: settings}, nil
}

func init() {
	simhome.FromFramework = func(home any) (simhome.Home, error) {
		h, ok := home.(Home)
		if !ok {
			return simhome.Home{}, fmt.Errorf("%T isn't a framework.Home", home)
		}
		return h.simulated()
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

func deviceKey(name string) ident.ID {
	return ident.ID{Namespace: wire.Namespace, Name: name}
}

func meta(d wire.Device, settings []wire.DeviceControl) hub.Meta {
	m := hub.Meta{DisplayName: d.Name, Room: d.Room, Instance: hostInstance}
	for _, c := range d.Arbitrated {
		// The host runs the arbiter, so only the name matters here to a claim. A budget plans around
		// the limits, which the home declared.
		entry := control.Entry{Capability: c}
		if i := slices.IndexFunc(settings, func(s wire.DeviceControl) bool {
			return s.Device == d.Name && s.Entry.Capability == c
		}); i >= 0 {
			entry = settings[i].Entry
		}
		m.Control = append(m.Control, entry)
	}
	return m
}

func seed(mirror *hub.Store, d wire.Device) {
	key := deviceKey(d.Name)
	for name, v := range d.Values {
		mirror.Changed(driver.Update{Device: key, Capability: name, Value: v})
	}
	if !d.LastSeen.IsZero() {
		mirror.Seen(key, d.LastSeen)
	}
	if d.Discovered {
		mirror.Discovered(key, driver.Info{Found: true, Capabilities: d.Capabilities})
	}
}

// discovered brings a device up to date, adding it if it's new since the Welcome. metas are the
// mirror's devices, which it replaces as a set. Only the read goroutine calls this, after the
// Welcome's seeding.
func discovered(mirror *hub.Store, metas map[ident.ID]hub.Meta, settings []wire.DeviceControl,
	d wire.Device,
) {
	metas[deviceKey(d.Name)] = meta(d, settings)
	mirror.SetDevices(hostInstance, metas)
	for name, v := range d.Values {
		changed(mirror, wire.Message{Device: d.Name, Capability: name, Value: v})
	}
	mirror.Discovered(deviceKey(d.Name), driver.Info{Found: true, Capabilities: d.Capabilities})
	if !d.LastSeen.IsZero() {
		mirror.Seen(deviceKey(d.Name), d.LastSeen)
	}
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

// logOutput is where the process logs. oikade dev and replay use standard output for something else,
// so its log stays out of the way there.
func logOutput() io.Writer {
	if len(os.Args) > 1 && (os.Args[1] == wire.RoleDev || os.Args[1] == wire.RoleReplay) {
		return io.Discard
	}
	return os.Stdout
}

// Secret reads a credential set with oikade secret set, such as an API key for r.Do. ok is false if
// there's no such secret, as under oikade dev. Never log the result.
func Secret(name string) (value string, ok bool, err error) {
	return plugin.ReadSecret(plugin.Environment{SecretsDir: os.Getenv(contractv1.EnvSecretsDir)}, name)
}
