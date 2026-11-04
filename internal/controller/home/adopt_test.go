package home

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Desk lamp":               "desk-lamp",
		"  Kitchen / Island ":     "kitchen-island",
		"Lounge Lamp 2":           "lounge-lamp-2",
		"Café":                    "caf",
		"---":                     "",
		strings.Repeat("ab ", 40): strings.TrimRight(strings.Repeat("ab-", 21), "-"),
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdoptedName(t *testing.T) {
	taken := map[string]bool{}
	take := func(d homev1alpha1.DiscoveredDevice) string {
		n := adoptedName(d, taken)
		taken[n] = true
		return n
	}
	got := []string{
		take(homev1alpha1.DiscoveredDevice{ID: "a", Name: "Lamp", Room: "Study"}),
		take(homev1alpha1.DiscoveredDevice{ID: "b", Name: "Lamp", Room: "Hall"}),
		take(homev1alpha1.DiscoveredDevice{ID: "c", Name: "Lamp", Room: "Study"}),
		take(homev1alpha1.DiscoveredDevice{ID: "d", Name: "Lamp", Room: "Study"}),
		take(homev1alpha1.DiscoveredDevice{ID: "e", Name: "Lamp"}),
		take(homev1alpha1.DiscoveredDevice{ID: "3f2a9c71", Model: "LCA001"}),
		take(homev1alpha1.DiscoveredDevice{ID: "0x00158d0001a2b3c4"}),
	}
	want := []string{"lamp", "hall-lamp", "study-lamp", "study-lamp-2", "lamp-2", "lca001-9c71", "device-b3c4"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("names = %v, want %v", got, want)
			break
		}
	}
}

func TestRoomFor(t *testing.T) {
	rooms := []homev1alpha1.Room{
		{ObjectMeta: metav1.ObjectMeta{Name: "study"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "lounge"}, Spec: homev1alpha1.RoomSpec{DisplayName: "Living room"}},
	}
	for in, want := range map[string]string{"Study": "study", "living room": "lounge", "Garage": "", "": ""} {
		got := roomFor(in, rooms)
		if (got == nil && want != "") || (got != nil && got.Name != want) {
			t.Errorf("roomFor(%q) = %v, want %q", in, got, want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	if got := displayName(" " + strings.Repeat("é", 70) + " "); got != strings.Repeat("é", 64) {
		t.Errorf("displayName cut to %d characters", len([]rune(got)))
	}
}
