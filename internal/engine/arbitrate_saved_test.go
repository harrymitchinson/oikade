package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/harrymitchinson/oikade/internal/control"
	"github.com/harrymitchinson/oikade/internal/ident"
	"github.com/harrymitchinson/oikade/internal/logging"
	"github.com/harrymitchinson/oikade/internal/store"
)

// A device whose driver hasn't attached has no arbiters, so its claims are only on disk. A deploy
// still has to see them, and to be able to end them.
func TestClaimsOnlyOnDiskCountAndEnd(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	controls := &Stored[ControlState]{Values: data.Namespace(ControlStatesNamespace), Log: logging.Discard()}
	valve := ident.ID{Name: "valve"}
	st := ControlState{"on": control.State{Claims: []control.Claim{
		{Source: "Rule/leak", Level: control.Safety, Value: 0},
		{Source: "Rule/other", Level: control.Override, Value: 1},
	}}}
	if err := controls.Put(context.Background(), valve, st); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Controls: controls, Log: logging.Discard()}
	e.Init()

	held := e.RuleClaims()
	if held["leak"] != control.Safety || held["other"] != control.Override {
		t.Fatalf("got %v", held)
	}
	if err := e.EndRule(context.Background(), "leak"); err != nil {
		t.Fatal(err)
	}
	held = e.RuleClaims()
	if _, ok := held["leak"]; ok || held["other"] != control.Override {
		t.Fatalf("got %v", held)
	}
	saved, _ := controls.Get(valve)
	raw, _ := json.Marshal(saved)
	if len(saved["on"].Claims) != 1 {
		t.Fatalf("got %s", raw)
	}
}
