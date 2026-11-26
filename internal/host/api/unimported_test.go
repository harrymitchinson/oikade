package api

import (
	"slices"
	"testing"

	"oikade.dev/internal/registry"
)

func TestUnimportedListsOnlyDevicesOfPluginsNothingRuns(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{ID: "lamp", Driver: "plugin/relays"},
		{ID: "fan", Driver: "plugin/relays"},
		{ID: "gone", Driver: "plugin/relays", Removed: true},
		{ID: "blind", Driver: "plugin/shades"},
		{ID: "socket", Driver: "plugin/outdoor"},
		{ID: "kettle", Driver: "simulated/home"},
	}}
	got := UnimportedOf(list, []string{"shades"}, []string{"outdoor"}, func(key string) bool { return key == "relays/fan" })
	want := []Unimported{
		{Device: "relays/fan", Ref: "hardware/relays/fan", Plugin: "relays", Safety: true},
		{Device: "relays/lamp", Ref: "hardware/relays/lamp", Plugin: "relays"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := UnimportedOf(list, []string{"relays", "shades"}, []string{"outdoor"}, nil); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}
