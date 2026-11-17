package oikade

import (
	"strings"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/internal/host/wire"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

var zigbee = func(plugin.Environment, []contractv1.DeclaredDevice) (plugin.Handler, error) { return nil, nil }

func TestADeclaredDeviceGoesToTheHost(t *testing.T) {
	h := decide(func(h *Home) {
		h.Plugin("zigbee", zigbee)
		h.Device("porch-light").Hardware("zigbee", "0x00178801049a2b3c").Room("porch").Is(OnOffLight)
		h.Device("greenhouse-fan").Room("greenhouse")
	})
	if _, _, err := h.compile(); err != nil {
		t.Fatal(err)
	}
	want := []wire.DeviceDeclaration{
		{Name: "porch-light", Plugin: "zigbee", ID: "0x00178801049a2b3c", Room: "porch", DeviceType: "OnOffLight"},
		{Name: "greenhouse-fan", Room: "greenhouse"},
	}
	got := h.manifest().Inventory
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %+v, want %+v", got, want)
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
