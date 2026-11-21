package engine

import (
	"testing"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/hub"
	"oikade.dev/internal/ident"
)

func TestAMovedDeviceKeepsItsClaimsUnderItsNewName(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	from, to := ident.ID{Name: "towel-rail"}, ident.ID{Name: "bath-rail"}
	if w := winner(r.status(from.Name, capability.On)); w != "Rule/rail Normal" {
		t.Fatalf("winner %q before the move", w)
	}
	r.settle()
	done, err := r.e.MoveDevices(t.Context(), map[ident.ID]ident.ID{from: to}, func(s string) string { return s + "!" })
	if err != nil {
		t.Fatal(err)
	}
	// Until the store renames it, the old name gets no arbiters, so nothing decides it afresh.
	r.e.ControlChanged(from)
	r.settle()
	r.e.mu.Lock()
	arbs := len(r.e.arbs[from])
	r.e.mu.Unlock()
	if arbs != 0 {
		t.Fatal("the old name got arbiters while it moved")
	}
	snap, _ := r.store.Get(from)
	r.store.Rename(map[ident.ID]hub.Move{from: {To: to, Meta: snap.Meta}})
	done()
	r.settle()
	if _, ok := r.e.Controls.Get(from); ok {
		t.Error("the old name's claims stayed")
	}
	saved, _ := r.e.Controls.Get(to)
	if len(saved[capability.On].Claims) == 0 || saved[capability.On].Claims[0].Source != "Rule/rail!" {
		t.Errorf("saved under the new name: %+v", saved)
	}
	if w := winner(r.status(to.Name, capability.On)); w != "Rule/rail! Normal" {
		t.Errorf("winner %q after the move, want the hold's claim, its source renamed", w)
	}
}

func TestAMoveThatCantSaveLeavesTheClaimsWhereTheyWere(t *testing.T) {
	r := railRig(t, holdRule(t, "rail", railHold))
	from, to := ident.ID{Name: "towel-rail"}, ident.ID{Name: "bath-rail"}
	r.settle()
	r.e.saveMu.Lock()
	r.e.mu.Lock()
	r.e.Controls = &failingFor{States: r.e.Controls, broken: to}
	r.e.mu.Unlock()
	r.e.saveMu.Unlock()
	done, err := r.e.MoveDevices(t.Context(), map[ident.ID]ident.ID{from: to}, func(s string) string { return s })
	if err == nil {
		t.Fatal("the move saved")
	}
	done()
	r.settle()
	if _, ok := r.e.Controls.Get(to); ok {
		t.Error("claims were left under the new name")
	}
	if saved, _ := r.e.Controls.Get(from); len(saved[capability.On].Claims) == 0 {
		t.Errorf("the old name's claims weren't put back: %+v", saved)
	}
	if w := winner(r.status(from.Name, capability.On)); w != "Rule/rail Normal" {
		t.Errorf("winner %q, want the hold still holding it", w)
	}
}
