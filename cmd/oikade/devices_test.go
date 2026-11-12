package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

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
	}}, map[string]bool{"boiler": true}, nil)
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[1], "plugin not imported") || strings.Contains(lines[2], "plugin not imported") {
		t.Errorf("got\n%s", out.String())
	}
}

func TestDevicesSayWhatsWrongWithThem(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{Name: "porch-light", ID: "1000aabbcc", Driver: "plugin/ewelink-lan", Endpoint: 2},
		{Name: "hall-light", ID: "1000ddeeff", Driver: "plugin/ewelink-lan", Endpoint: 3},
	}}
	problems := map[string]string{"porch-light": "no LAN key: oikade secret set 1000aabbcc"}

	var out bytes.Buffer
	printDevices(&out, list, nil, problems)
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[2], problems["porch-light"]) || strings.Contains(lines[1], "no LAN key") {
		t.Errorf("got\n%s", out.String())
	}

	raw, err := json.Marshal(listed(list, nil, problems))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"problem":"no LAN key: oikade secret set 1000aabbcc"`) {
		t.Errorf("got %s", raw)
	}
	if strings.Count(string(raw), `"problem"`) != 1 {
		t.Errorf("a device with no problem should leave it out: %s", raw)
	}
}

func TestStatusSaysWhatsWrongWithADevice(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, io.Discard, &api.Status{Devices: []api.Device{
		{
			Name: "porch-light", Problem: "no LAN key: oikade secret set 1000aabbcc",
			Capabilities: []api.Capability{{Name: "on"}},
		},
		{Name: "shed-relay", Problem: "bad parameters"},
	}}, false, time.Now())
	for _, want := range []string{
		"porch-light  on  -  no LAN key: oikade secret set 1000aabbcc",
		"shed-relay   -   -  bad parameters",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
}
