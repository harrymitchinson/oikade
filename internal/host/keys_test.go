package host

import (
	"testing"

	"oikade.dev/internal/engine"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
	"oikade.dev/internal/testkit"
)

// A store kept by devices' names starts its state and flow runs again, once: what's saved after that
// is by hardware, and stays. A new one has nothing to start again.
func TestAStoreKeptByNameStartsAgainOnce(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	// A store no host kept a device in is new, and keeps what's put in it.
	if err := data.Namespace(engine.HoldStatesNamespace).Put("seeded", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if err := fresh.Namespace(engine.FireStatesNamespace).Put("morning", []byte(`"2026-11-26T07:00:00Z"`)); err != nil {
		t.Fatal(err)
	}
	if err := keyByHardware(fresh, logging.Discard()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := fresh.Namespace(engine.FireStatesNamespace).Get("morning"); !ok {
		t.Error("a new store lost what was put in it")
	}

	reg, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Report(registry.Report{ID: "v", Driver: "plugin/relays", Name: "valve"}); err != nil {
		t.Fatal(err)
	}
	keep := func() {
		t.Helper()
		if err := data.Namespace(engine.HoldStatesNamespace).Put("valve on leak", []byte(`{"active":true,"latched":true}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := data.Journal().Append("drain/1", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	keep()
	log := testkit.Logs(t)
	if err := keyByHardware(data, log.Logger); err != nil {
		t.Fatal(err)
	}
	if !log.Has(`msg="Started claims, latches and flow runs again, as the store kept them by device name"`, "states=2", "runs=1") {
		t.Errorf("logged:\n%v", log.Lines())
	}
	if _, ok, _ := data.Namespace(engine.HoldStatesNamespace).Get("valve on leak"); ok {
		t.Error("the latch stayed")
	}
	if runs, _ := data.Journal().Runs(); len(runs) != 0 {
		t.Errorf("runs %v stayed", runs)
	}

	keep()
	if err := keyByHardware(data, log.Logger); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := data.Namespace(engine.HoldStatesNamespace).Get("valve on leak"); !ok {
		t.Error("a second start dropped what was saved by hardware")
	}
}
