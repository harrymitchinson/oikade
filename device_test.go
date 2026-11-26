package oikade

import (
	"encoding/json"
	"strings"
	"testing"

	"oikade.dev/contract/v1"
	"oikade.dev/internal/host/wire"
	"oikade.dev/sdk/plugin"
)

var zigbee = func(plugin.Environment, []contract.DeclaredDevice) (plugin.Handler, error) { return nil, nil }

func TestADeclaredDeviceGoesToTheHost(t *testing.T) {
	h := decide(func(h *Home) {
		h.Plugin("zigbee", zigbee)
		h.Device("porch-light").Hardware("zigbee", "0x00178801049a2b3c").Room("porch").Is(OnOffLight)
		h.Device("greenhouse-fan").Room("greenhouse")
		h.Device("attic-fan").Room("")
		h.Device("shed-fan")
	})
	if _, _, err := h.compile(); err != nil {
		t.Fatal(err)
	}
	want := []wire.DeviceDeclaration{
		{Name: "porch-light", Plugin: "zigbee", ID: "0x00178801049a2b3c", Room: new("porch"), DeviceType: "OnOffLight"},
		{Name: "greenhouse-fan", Room: new("greenhouse")},
		{Name: "attic-fan", Room: new("")},
		{Name: "shed-fan"},
	}
	got := h.manifest().Inventory
	g, _ := json.Marshal(got)
	if w, _ := json.Marshal(want); string(g) != string(w) {
		t.Errorf("got %s, want %s", g, w)
	}
	if r := wire.Declarations(got); r[0].Driver != "plugin/zigbee" || r[1].Driver != "" {
		t.Errorf("the registry gets %+v", r)
	}
}

func TestDeclaredDevicesAreChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		add  func(*Home)
		want string
	}{
		"name twice": {func(h *Home) { h.Device("lamp"); h.Device("lamp") }, "declared twice"},
		"hardware twice": {func(h *Home) {
			h.Device("a").Hardware("zigbee", "0x1")
			h.Device("b").Hardware("zigbee", "0x1")
		}, "both declared"},
		"bad name":   {func(h *Home) { h.Device("Porch Light") }, "isn't a device name"},
		"not a type": {func(h *Home) { h.Device("lamp").Is(Thermostat) }, "isn't one of"},
		"no plugin":  {func(h *Home) { h.Device("lamp").Hardware("hue", "0x1") }, "doesn't import"},
		"no ID":      {func(h *Home) { h.Device("lamp").Hardware("zigbee", "") }, "half its hardware"},
	} {
		h := decide(func(h *Home) { h.Plugin("zigbee", zigbee); tc.add(h) })
		if _, _, err := h.compile(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}
