package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/clock"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/engine"
	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/hub"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
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

	s := &apiServer{srv: &server{engine: eng, devices: devices, outside: &writes{err: errors.New("no answer")}, feed: newFeed()}}
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
