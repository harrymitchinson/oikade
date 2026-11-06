package framework

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/internal/hub"
)

// dialTimeout bounds opening a session with the host.
const dialTimeout = 10 * time.Second

var hostInstance = driver.InstanceKey{Driver: "host"}

func run(h Home, log logr.Logger) error {
	rules, err := h.compile()
	if err != nil {
		return err
	}
	if len(os.Args) < 2 || os.Args[1] != wire.RoleAutomations {
		return errors.New("this is a home: run it with oikade host")
	}
	socket := os.Getenv(wire.SocketEnv)
	if socket == "" {
		return fmt.Errorf("%s isn't set: run this with oikade host", wire.SocketEnv)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Key.Name)
	}
	dial, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	client, welcome, err := wire.Dial(dial, socket, names)
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
	eng.Init()
	mirror.Listeners = []driver.Sink{eng}
	for _, r := range rules {
		eng.Load(r)
	}

	pings := make(chan uint64, 1)
	go answerPings(ctx, client, eng, pings)
	go client.Listen(wire.Events{
		Changed: func(m wire.Message) { changed(mirror, m) },
		Discovered: func(d wire.Device) {
			mirror.Discovered(deviceKey(d.Name), driver.Info{Found: true, Capabilities: d.Capabilities})
		},
		Ping: func(id uint64) {
			select {
			case pings <- id:
			default:
			}
		},
	})

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	log.Info("Loaded rules", "rules", len(rules), "devices", len(welcome.Devices))
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
