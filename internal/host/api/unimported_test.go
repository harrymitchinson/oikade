package api

import (
	"slices"
	"testing"

	"github.com/harrymitchinson/oikade/internal/registry"
)

func TestUnimportedListsOnlyDevicesOfPluginsNothingRuns(t *testing.T) {
	list := registry.Snapshot{Devices: []registry.Device{
		{Name: "lamp", Driver: "plugin/relays"},
		{Name: "fan", Driver: "plugin/relays"},
		{Name: "gone", Driver: "plugin/relays", Removed: true},
		{Name: "blind", Driver: "plugin/shades"},
		{Name: "socket", Driver: "plugin/outdoor"},
		{Name: "kettle", Driver: "simulated/home"},
		{Name: "reserved"},
	}}
	got := UnimportedOf(list, []string{"shades"}, []string{"outdoor"}, func(name string) bool { return name == "fan" })
	want := []Unimported{{Device: "fan", Plugin: "relays", Safety: true}, {Device: "lamp", Plugin: "relays"}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := UnimportedOf(list, []string{"relays", "shades"}, []string{"outdoor"}, nil); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}
