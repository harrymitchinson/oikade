package host

import (
	"testing"

	"oikade.dev/internal/engine"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
	"oikade.dev/internal/testkit"
)

// A store kept by devices' names starts again what it kept under the name of a device now known by
// its hardware, once, and keeps the rest: a reserved device's, whose key is its name, and a flow's
// that names no such device.
func TestAStoreKeptByNameStartsAgainWhatsUnderAHardwaresName(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	put := func(namespace, key, value string) {
		t.Helper()
		if err := data.Namespace(namespace).Put(key, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	// The registry as a host saved it before devices were their hardware: the fan is a reserved name.
	put(registry.Namespace, "state", `{"next":4,"devices":[`+
		`{"name":"valve","driver":"plugin/relays","id":"v1","endpoint":2},{"name":"fan","endpoint":3}]}`)
	gone := map[string][]string{
		engine.ControlStatesNamespace: {"valve"},
		engine.HoldStatesNamespace:    {"valve on leak", "taps on leak"},
		engine.AverageStatesNamespace: {"valve on 1h"},
		pausedNamespace:               {"valve on leak"},
	}
	kept := map[string][]string{
		engine.ControlStatesNamespace: {"fan"},
		engine.HoldStatesNamespace:    {"fan on humid"},
		engine.FireStatesNamespace:    {"morning"},
		pausedNamespace:               {"morning"},
	}
	for namespace, keys := range kept {
		for _, k := range keys {
			put(namespace, k, `{}`)
		}
	}
	for namespace, keys := range gone {
		for _, k := range keys {
			put(namespace, k, `{}`)
		}
	}
	// A group's case that writes the valve.
	put(engine.HoldStatesNamespace, "taps on leak", `{"active":true,"latched":true,"target":{"devices":["valve","tap"]}}`)
	journal := data.Journal()
	for run, step := range map[string]string{"drain/1": `{"id":"look valve on"}`, "morning/1": `{"id":"wait"}`, "valves/1": `{"id":"valves"}`} {
		if _, err := journal.Append(run, []byte(step)); err != nil {
			t.Fatal(err)
		}
	}

	log := testkit.Logs(t)
	if err := keyByHardware(data, log.Logger); err != nil {
		t.Fatal(err)
	}
	if !log.Has(`msg="Started claims, latches and flow runs again, as the store kept them by device name"`, "states=5", "runs=1") {
		t.Errorf("logged:\n%v", log.Lines())
	}
	for namespace, keys := range gone {
		for _, k := range keys {
			if _, ok, _ := data.Namespace(namespace).Get(k); ok {
				t.Errorf("%s kept %s", namespace, k)
			}
		}
	}
	for namespace, keys := range kept {
		for _, k := range keys {
			if _, ok, _ := data.Namespace(namespace).Get(k); !ok {
				t.Errorf("%s lost %s", namespace, k)
			}
		}
	}
	if runs, _ := journal.Runs(); len(runs) != 2 {
		t.Errorf("runs %v, want morning's and valves'", runs)
	}

	// Once: what's under the valve's name from now on is another device's, a reserved one, say.
	put(engine.ControlStatesNamespace, "valve", `{}`)
	if err := keyByHardware(data, logging.Discard()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := data.Namespace(engine.ControlStatesNamespace).Get("valve"); !ok {
		t.Error("a second start dropped what was saved by hardware")
	}
}
