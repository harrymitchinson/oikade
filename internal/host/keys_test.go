package host

import (
	"encoding/json"
	"slices"
	"testing"

	"oikade.dev/internal/engine"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/logging"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
	"oikade.dev/internal/testkit"
)

// A store kept by devices' names starts again what it kept under the name of a device now known by
// its hardware, or of one reserved without hardware, once, and keeps a flow's that names no such
// device.
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
		engine.ControlStatesNamespace: {"valve", "fan"},
		engine.HoldStatesNamespace:    {"valve on leak", "taps on leak", "fan on humid"},
		engine.AverageStatesNamespace: {"valve on 1h"},
		pausedNamespace:               {"valve on leak"},
	}
	kept := map[string][]string{
		engine.FireStatesNamespace: {"morning"},
		pausedNamespace:            {"morning"},
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
	if !log.Has(`msg="Started claims, latches and flow runs again, as the store kept them by device name"`, "states=7", "runs=1") {
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

	// Once: what's under the valve's name after that stays.
	put(engine.ControlStatesNamespace, "valve", `{}`)
	if err := keyByHardware(data, logging.Discard()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := data.Namespace(engine.ControlStatesNamespace).Get("valve"); !ok {
		t.Error("a second start dropped what was saved by hardware")
	}
}

// The devices a home shows on Matter are kept by hardware, even by a host that keyed the rest by
// hardware already, and a name reserved without hardware among them goes.
func TestDevicesShownOnMatterAreKeptByHardware(t *testing.T) {
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	put := func(namespace, key string, value []byte) {
		t.Helper()
		if err := data.Namespace(namespace).Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	put(registry.Namespace, "state", []byte(`{"next":4,"devices":[`+
		`{"name":"garage-door","driver":"plugin/tahoma","id":"io://1/7","endpoint":2},{"name":"gate","endpoint":3}]}`))
	put(layoutNamespace, deviceKeys, []byte(keyedByHardware))
	hidden, err := json.Marshal([]wire.HiddenPlugin{{Plugin: "tahoma", Shown: []string{"garage-door", "gate", "tahoma/9"}}})
	if err != nil {
		t.Fatal(err)
	}
	put(optionsNamespace, hiddenKey, hidden)

	if err := keyByHardware(data, logging.Discard()); err != nil {
		t.Fatal(err)
	}
	options, err := loadOptions(data.Namespace(optionsNamespace))
	if err != nil {
		t.Fatal(err)
	}
	got := options.hiddenFromMatter()
	want := []string{registry.KeyOf("plugin/tahoma", "io://1/7"), "tahoma/9"}
	slices.Sort(want)
	if len(got) != 1 || !slices.Equal(got[0].Shown, want) {
		t.Errorf("shown %+v, want %q", got, want)
	}
}
