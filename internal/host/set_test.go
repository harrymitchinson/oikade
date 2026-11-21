package host

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/clock"
	"oikade.dev/internal/control"
	"oikade.dev/internal/driver"
	"oikade.dev/internal/engine"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
	"oikade.dev/internal/logging"
)

// An oikade set that never reached the device leaves the device's next change its own: the wall
// switch flipped after it is somebody at the switch, not the command.
func TestAFailedSetLeavesTheDevicesNextChangeItsOwn(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	now := clock.NewFake(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	eng := &engine.Engine{
		Log: logging.Discard(), Clock: now,
		Controls: &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()},
	}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store, eng.Setter = devices, &writes{}
	eng.Init()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	dev := key("fan")
	entry := control.Entry{
		Capability: capability.On, Default: []byte("false"), External: control.NameOverride, OverrideHold: "1h",
	}
	devices.SetDevices(simInstance, map[ident.ID]hub.Meta{dev: {Instance: simInstance, Control: []control.Entry{entry}}})
	devices.Discovered(dev, driver.Info{Found: true, Capabilities: []string{capability.On}})
	devices.Changed(driver.Update{Device: dev, Capability: capability.On, Value: false, Time: now.Now()})
	idle(t, eng)

	s := &apiServer{srv: &server{engine: eng, devices: devices, setter: &writes{err: errors.New("no answer")}, feed: newFeed()}}
	resp := s.set(ctx, api.Request{Type: api.TypeSet, Device: "fan", Capability: capability.On, Value: true})
	if resp.Error == "" {
		t.Fatalf("the failure was lost: %+v", resp)
	}

	devices.Changed(driver.Update{Device: dev, Capability: capability.On, Value: true, Previous: false, Time: now.Now()})
	// The arbiter wrote its Default when it started, so the change may wait for that write to settle.
	idle(t, eng)
	now.Step(capability.DefaultSettle)
	idle(t, eng)
	for _, c := range eng.ControlOf(dev) {
		for _, claim := range c.Claims {
			if claim.Winner && claim.Source != "" {
				if claim.Source != engine.SourceDevice {
					t.Fatalf("the switch's change was taken for %s's", claim.Source)
				}
				return
			}
		}
	}
	t.Fatalf("no override: %+v", eng.ControlOf(dev))
}

// idle waits for the engine to have nothing left to do until its clock moves.
func idle(t *testing.T, eng *engine.Engine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !eng.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("the engine didn't settle")
		}
		time.Sleep(time.Millisecond)
	}
}

// Every write goes into the log with whose it is: a rule's, as your process says of its own, a
// controller's, and nobody's when nothing says. Your process can't write as anyone but a rule.
func TestAWriteIsLoggedWithWhoseItIs(t *testing.T) {
	data := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = data.Close() })
	eng := &engine.Engine{
		Log:      logging.Discard(),
		Controls: &engine.Stored[engine.ControlState]{Values: data.Namespace(engine.ControlStatesNamespace), Log: logging.Discard()},
	}
	devices := hub.NewStore(logging.Discard(), eng)
	eng.Store = devices
	eng.Init()
	var events []api.Event
	recorded := recordedSetter{emit: func(e api.Event) { events = append(events, e) }, setter: &writes{}}
	srv := &server{ctx: t.Context(), setter: recorded}
	for _, source := range []string{"Rule/fan on humid", "Rule/morning", control.SourceMatter, ""} {
		m := wire.Message{Type: wire.TypeSet, Device: "fan", Capability: capability.On, Value: true, Source: source}
		if reply := srv.answer(m); reply.Error != "" {
			t.Fatal(reply.Error)
		}
	}
	bridge := &matterWrites{engine: eng, devices: devices, setter: recorded, restore: func(ident.ID, string, any) {}}
	if err := bridge.Set(t.Context(), key("fan"), capability.On, false); err != nil {
		t.Fatal(err)
	}
	if err := recorded.Set(driver.WithWriter(t.Context(), control.SourceCommand), key("fan"), capability.On, true); err != nil {
		t.Fatal(err)
	}
	by := make([]string, 0, len(events))
	for _, e := range events {
		if e.Type != api.EventWrite || e.Device != "fan" || e.Capability != capability.On {
			t.Fatalf("event %+v", e)
		}
		by = append(by, e.By)
	}
	if want := []string{"case/fan/on/humid", "flow/morning", "", "", "matter", "command"}; !slices.Equal(by, want) {
		t.Fatalf("the writes are by %q, want %q", by, want)
	}
}
