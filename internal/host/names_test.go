package host

import (
	"testing"

	"oikade.dev/internal/capability"
	"oikade.dev/internal/control"
	"oikade.dev/internal/host/api"
	"oikade.dev/internal/host/wire"
	"oikade.dev/internal/registry"
	"oikade.dev/internal/store"
)

// namedHome is a registry with a lamp from a plugin, which has an ID that needs escaping, and a
// simulated fan, whose key is its name.
func namedHome(t *testing.T) *registry.Registry {
	t.Helper()
	data, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	r, err := registry.Open(data.Namespace(registry.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range []registry.Report{
		{ID: "io://1/2", Driver: "plugin/overkiz", Name: "terrace blind", Capabilities: []string{capability.On}},
		{ID: "fan", Driver: instanceName(simInstance), Name: "fan", Capabilities: []string{capability.On}},
	} {
		if _, _, err := r.Report(rep); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// An event keeps its devices by key, and a person reads their names, in its refs as in its text.
func TestAnEventNamesItsDevices(t *testing.T) {
	r := namedHome(t)
	const blind = "overkiz/io:%2F%2F1%2F2"
	got := namedEvent(api.Event{
		Type: api.EventWrite, Device: blind, Capability: capability.On,
		Ref: "device/overkiz%2Fio:%252F%252F1%252F2/on", By: "case/overkiz%2Fio:%252F%252F1%252F2/on/dusk",
	}, r)
	want := api.Event{Type: api.EventWrite, Device: "terrace-blind", Capability: capability.On,
		Ref: "device/terrace-blind/on", By: "case/terrace-blind/on/dusk"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	run := namedEvent(api.Event{Type: api.EventRule, Rule: blind + " on dusk", Result: "device/" + blind + "/on is unavailable"}, r)
	if run.Rule != "terrace-blind on dusk" || run.Result != "device/terrace-blind/on is unavailable" {
		t.Errorf("got %+v", run)
	}
	renamed := namedEvent(api.Event{Type: api.EventRename, Device: blind, Previous: "blind", Message: "terrace-blind"}, r)
	if renamed.Device != "terrace-blind" || renamed.Ref != "device/terrace-blind" || renamed.Previous != "blind" {
		t.Errorf("a rename reads %+v", renamed)
	}
	if fan := namedEvent(api.Event{Type: api.EventChange, Device: "fan", Ref: "device/fan/on"}, r); fan.Device != "fan" || fan.Ref != "device/fan/on" {
		t.Errorf("a simulated device reads %+v", fan)
	}
}

// What your process writes names a device by its key, escaped as a ref has it or not, as a why-not
// says it, and a person reads its name.
func TestTextYourProcessWroteNamesItsDevices(t *testing.T) {
	r := namedHome(t)
	for text, want := range map[string]string{
		"device/overkiz%2Fio:%252F%252F1%252F2/on is false":   "device/terrace-blind/on is false",
		"device/overkiz/io:%2F%2F1%2F2/on is false":           "device/terrace-blind/on is false",
		"case/overkiz%2Fio:%252F%252F1%252F2/on/dusk is held": "case/terrace-blind/on/dusk is held",
		"device/fan/on is true":                               "device/fan/on is true",
		"no refs":                                             "no refs",
	} {
		if got := namedText(text, r); got != want {
			t.Errorf("%q reads %q, want %q", text, got, want)
		}
	}
}

// A person names a device, or a case by its device, and the host finds it by its key.
func TestANameFindsItsKey(t *testing.T) {
	r := namedHome(t)
	if dev, ok := keyOfName(r, "terrace-blind"); !ok || dev.Name != "overkiz/io:%2F%2F1%2F2" {
		t.Errorf("terrace-blind is %v, %v", dev, ok)
	}
	if got := ruleKeyOf(r, "terrace-blind on dusk"); got != control.CaseKey("overkiz/io:%2F%2F1%2F2", "on", "dusk") {
		t.Errorf("the case is %q", got)
	}
	if got := ruleKeyOf(r, "lounge on dusk"); got != "lounge on dusk" {
		t.Errorf("a group's case is %q", got)
	}
	// A group named as a device forgotten was is the group.
	if err := r.Remove("terrace-blind"); err != nil {
		t.Fatal(err)
	}
	if got := ruleKeyOf(r, "terrace-blind on dusk"); got != "terrace-blind on dusk" {
		t.Errorf("a group named as a forgotten device has the case %q", got)
	}
	if _, ok := keyOfName(r, "nothing"); ok {
		t.Error("found a device nothing is called")
	}
}

// Before a deploy applies its declarations, what it says names devices as they'll be called, and
// hardware whose name goes to other hardware by its key.
func TestADeploySaysTheNamesItsDeclarationsGive(t *testing.T) {
	r := namedHome(t)
	names := declaredNames([]wire.DeviceDeclaration{{Name: "terrace-blind", Plugin: "overkiz", ID: "io://9/9"}}, registryNames(r))
	if got := names.Device("overkiz/io:%2F%2F9%2F9"); got != "terrace-blind" {
		t.Errorf("the new hardware is %q", got)
	}
	if got := names.Device("overkiz/io:%2F%2F1%2F2"); got != "overkiz/io:%2F%2F1%2F2" {
		t.Errorf("the old hardware is %q, want its key", got)
	}
	if got := names.Device("fan"); got != "fan" {
		t.Errorf("an undeclared device is %q", got)
	}
}
