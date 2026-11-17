package engine

import (
	"context"
	"slices"
	"sync"
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
			if err := controls.Put(t.Context(), pump, saved); err != nil {
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

// claimWatch takes claims as the host does, and records the hold's saved state as each arrives.
type claimWatch struct {
	states HoldStates
	key    ident.ID
	mu     sync.Mutex
	saved  []HoldState
}

func (w *claimWatch) Claim(context.Context, ident.ID, string, control.Claim) bool {
	st, _ := w.states.Get(w.key)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.saved = append(w.saved, st)
	return true
}

func (w *claimWatch) ReleaseClaim(context.Context, ident.ID, string, string) bool { return true }

func (w *claimWatch) ReleaseClaims(context.Context, string, control.Level) error { return nil }

func (w *claimWatch) claims() []HoldState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.saved)
}

// OIK-299: a start's claim outlives your process, so the hold is saved as active, latch and all,
// before the claim reaches the host. A process killed in between loads it active and claims again,
// even with the overheat past: it stays latched off rather than inactive with nothing to end the
// claim.
func TestAHoldIsSavedActiveBeforeItsStartClaims(t *testing.T) {
	key := ident.ID{Name: "workshop-overheat"}
	holds := newMemoryStates[HoldState]()
	heater := map[string][]control.Entry{"workshop-heater": {entry(capability.On, "true", control.NameOverride, "1h")}}
	w := &claimWatch{states: holds, key: key}
	r := startRigWith(t, rigOptions{
		now: local("2026-11-17 09:00 GMT"), holds: holds, arbitration: w, control: heater,
		values: map[string]map[string]any{"workshop-temp": {capability.Temperature: 50.0}},
	}, holdRule(t, "workshop-overheat", overheat))
	claims := w.claims()
	if len(claims) != 1 || !claims[0].Active || !claims[0].Latched {
		t.Fatalf("saved as %+v when the claims came, want active and latched", claims)
	}
	r.stop()

	// That state is what a process killed right after the claim leaves, and the overheat has
	// passed by the restart.
	again := &claimWatch{states: holds, key: key}
	r = startRigWith(t, rigOptions{
		now: local("2026-11-17 09:01 GMT"), holds: holds, arbitration: again, control: heater,
		values: map[string]map[string]any{"workshop-temp": {capability.Temperature: 20.0}},
	}, holdRule(t, "workshop-overheat", overheat))
	if n := len(again.claims()); n != 1 {
		t.Fatalf("claimed %d times after the restart, want once", n)
	}
	if st, _ := holds.Get(key); !st.Active || !st.Latched {
		t.Errorf("after the restart: %+v, want still active and latched", st)
	}
	r.expect()
}

// A hold whose start is a plain write is saved after it, since the write ends with your process:
// one killed in between writes the start again if it's still due.
func TestAHoldThatOnlyWritesIsSavedAfterItsStart(t *testing.T) {
	key := ident.ID{Name: "workshop-overheat"}
	holds := newMemoryStates[HoldState]()
	r := startRigWith(t, rigOptions{
		now: local("2026-11-17 09:00 GMT"), holds: holds,
		values: map[string]map[string]any{"workshop-temp": {capability.Temperature: 50.0}},
	})
	var before HoldState
	r.setter.mu.Lock()
	r.setter.before = func() { before, _ = holds.Get(key) }
	r.setter.mu.Unlock()
	r.e.Load(holdRule(t, "workshop-overheat", overheat))
	r.settle()
	r.expect(call{"workshop-heater", capability.On, false})
	if before.Active {
		t.Errorf("saved as %+v before the write, want inactive", before)
	}
	if st, _ := holds.Get(key); !st.Active {
		t.Errorf("saved as %+v after it, want active", st)
	}
}

// A group that claims on one member and writes the other is saved after its start too: loaded
// active, it would only claim again, and the member it writes would never get its start.
func TestAGroupThatAlsoWritesIsSavedAfterItsStart(t *testing.T) {
	key := ident.ID{Name: "workshop-overheat"}
	holds := newMemoryStates[HoldState]()
	w := &claimWatch{states: holds, key: key}
	group := overheat
	group.Device, group.Devices = "", []string{"workshop-heater", "water-heater"}
	r := startRigWith(t, rigOptions{
		now: local("2026-11-17 09:00 GMT"), holds: holds, arbitration: w,
		control: map[string][]control.Entry{"workshop-heater": {entry(capability.On, "true", control.NameOverride, "1h")}},
		values:  map[string]map[string]any{"workshop-temp": {capability.Temperature: 50.0}},
	}, holdRule(t, "workshop-overheat", group))
	r.expect(call{"water-heater", capability.On, false})
	if claims := w.claims(); len(claims) != 1 || claims[0].Active {
		t.Fatalf("saved as %+v when the claims came, want one claim with the hold inactive", claims)
	}
	if st, _ := holds.Get(key); !st.Active {
		t.Errorf("saved as %+v after the start, want active", st)
	}
}
