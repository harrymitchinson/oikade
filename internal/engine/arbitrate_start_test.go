package engine

import (
	"context"
	"testing"
	"time"

	"github.com/harrymitchinson/oikade/internal/capability"
	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/ident"
)

// The pump was off when the host stopped, held off by a rule a deploy has since removed, and it
// comes back on. Its first report after the start is what it is now, not a tap, whichever comes
// first of it and the removal that's due at startup, so the pump is written off again with no
// Override. A report after that is a tap.
func TestTheFirstReportAfterAStartIsNoOverride(t *testing.T) {
	pump := ident.ID{Name: "pump"}
	orders := map[string]func(r *rig){
		"reported before the engine runs": func(r *rig) { r.endRule("pump-quiet") },
		"reported before the removal": func(r *rig) {
			r.change("pump", capability.On, true)
			r.endRule("pump-quiet")
		},
		"reported after the removal": func(r *rig) {
			r.endRule("pump-quiet")
			r.change("pump", capability.On, true)
		},
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			states := newStoreBackend(t)
			holds := states.holds(t) // opens the store controls uses
			controls := states.controls(t)
			saved := ControlState{capability.On: control.State{Value: 0, Known: true, Claims: []control.Claim{
				{Source: ruleSource("pump-quiet"), Level: control.Normal, Value: 0},
			}}}
			if err := controls.Put(context.Background(), pump, saved); err != nil {
				t.Fatal(err)
			}
			o := rigOptions{
				now: local("2026-11-17 09:00 GMT"), holds: holds, controls: controls,
				control: map[string][]control.Entry{"pump": {entry(capability.On, "false", control.NameOverride, "1h")}},
			}
			if name == "reported before the engine runs" {
				o.values = map[string]map[string]any{"pump": {capability.On: true}}
			}
			r := startRigWith(t, o)
			order(r)
			if w := winner(r.status("pump", capability.On)); w != control.NameDefault {
				t.Errorf("the pump is held by %s", w)
			}
			r.expect(call{"pump", capability.On, false})

			r.change("pump", capability.On, false)
			r.clock.Step(time.Minute)
			r.change("pump", capability.On, true)
			if w := winner(r.status("pump", capability.On)); w != "Device Override" {
				t.Errorf("a tap after the start is held by %s", w)
			}
		})
	}
}

// endRule ends a rule's claims, as a deploy that removed it does.
func (r *rig) endRule(rule string) {
	r.t.Helper()
	if err := r.e.EndRule(context.Background(), rule); err != nil {
		r.t.Fatal(err)
	}
	r.settle()
}
