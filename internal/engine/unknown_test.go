package engine

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/driver"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// TestAnOffDelayThatEndsWhileWhenIsUnknownLeavesTheEngineFree is OIK-216: a hold's off-delay
// ran out while its when couldn't be evaluated, as when a room it reads loses its only sensor. Its
// next look was the delay's end, already past, so Run fired it again and again and never took
// another change.
func TestAnOffDelayThatEndsWhileWhenIsUnknownLeavesTheEngineFree(t *testing.T) {
	var unknown atomic.Bool
	occupied := isTrue("hall-motion", capability.Occupied)
	light := holdRule(t, "hall-light", Hold{
		Device: "hall-light", Capability: capability.On, Value: true, OffDelay: 5 * time.Minute,
		When: func(v Vars) (bool, error) {
			if unknown.Load() {
				return false, errors.New("the hall has no occupancy reading")
			}
			return occupied(v)
		},
	})
	fan := holdRule(t, "bathroom-humid", Hold{
		Device: "bathroom-fan", Capability: capability.On, Value: true,
		When: above("bathroom-sensor", capability.Humidity, 70),
	})
	values := map[string]map[string]any{
		"hall-motion":     {capability.Occupied: true},
		"bathroom-sensor": {capability.Humidity: 50.0},
	}
	r := startRig(t, local("2026-10-21 20:00 BST"), nil, values, nil, light, fan)
	r.expect(call{"hall-light", capability.On, true})
	r.at("2026-10-21 20:10 BST")
	r.change("hall-motion", capability.Occupied, false) // The light goes off at 20:15.
	r.at("2026-10-21 20:12 BST")
	unknown.Store(true)
	r.change("hall-motion", capability.Occupied, false)

	// The fan shows Run still takes changes. Then settle shows the light isn't due all the time.
	r.clock.SetTime(local("2026-10-21 20:16 BST"))
	r.store.Changed(driver.Update{
		Device: ident.ID{Namespace: ns, Name: "bathroom-sensor"}, Capability: capability.Humidity, Value: 75.0,
	})
	want := fmt.Sprint([]call{{"hall-light", capability.On, true}, {"bathroom-fan", capability.On, true}})
	poll(t, "the fan to follow the humidity", func() bool { return fmt.Sprint(r.setter.got()) == want })
	r.settle()

	// The light kept its state while when was unknown, and goes off once it's known again.
	unknown.Store(false)
	r.change("hall-motion", capability.Occupied, false)
	r.expect(call{"hall-light", capability.On, true}, call{"bathroom-fan", capability.On, true},
		call{"hall-light", capability.On, false})
}
