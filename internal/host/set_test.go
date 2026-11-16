package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
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
	eng := &engine.Engine{
		Log:      logging.Discard(),
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
	devices.Changed(driver.Update{Device: dev, Capability: capability.On, Value: false, Time: time.Now()})

	s := &apiServer{srv: &server{engine: eng, devices: devices, outside: &writes{err: errors.New("no answer")}, feed: newFeed()}}
	resp := s.set(ctx, api.Request{Type: api.TypeSet, Device: "fan", Capability: capability.On, Value: true})
	if resp.Error == "" {
		t.Fatalf("the failure was lost: %+v", resp)
	}

	devices.Changed(driver.Update{Device: dev, Capability: capability.On, Value: true, Previous: false, Time: time.Now()})
	// The arbiter wrote its Default when it started, so the change may wait for that write to settle.
	deadline := time.Now().Add(capability.DefaultSettle + 5*time.Second)
	for {
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
		if time.Now().After(deadline) {
			t.Fatalf("no override: %+v", eng.ControlOf(dev))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
