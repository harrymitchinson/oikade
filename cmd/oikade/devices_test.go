package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/harrymitchinson/oikade/internal/host/api"
	"github.com/harrymitchinson/oikade/internal/registry"
)

func TestUnimportedDevicesSayHowToRemoveThemAndCallOutSafety(t *testing.T) {
	var out bytes.Buffer
	printUnimported(&out, []api.Unimported{
		{Device: "awning", Plugin: "shades"},
		{Device: "boiler", Plugin: "relays", Safety: true},
		{Device: "heater", Plugin: "relays"},
	})
	want := `These devices stay on record, offline, because your home doesn't import their plugin:
  relays: boiler, heater
    A Safety claim holds boiler, and removing it ends that claim
    Remove them with oikade devices remove --plugin relays
  shades: awning
    Remove them with oikade devices remove --plugin shades
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
	out.Reset()
	printUnimported(&out, nil)
	if out.Len() != 0 {
		t.Errorf("got %q", out.String())
	}
}

func TestDevicesMarkThoseWhosePluginIsntImported(t *testing.T) {
	var out bytes.Buffer
	printDevices(&out, registry.Snapshot{Devices: []registry.Device{
		{Name: "boiler", ID: "b", Driver: "plugin/relays", Endpoint: 2},
		{Name: "lamp", ID: "l", Driver: "plugin/shades", Endpoint: 3},
	}}, map[string]bool{"boiler": true})
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[1], "plugin not imported") || strings.Contains(lines[2], "plugin not imported") {
		t.Errorf("got\n%s", out.String())
	}
}
