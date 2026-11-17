package oikade

import (
	"strings"
	"testing"

	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

func TestEveryNameFollowsTheDeviceRule(t *testing.T) {
	on := []Change{Bool{Device: "hall", Capability: Open}}
	made := func(plugin.Environment, []contractv1.DeclaredDevice) (plugin.Handler, error) { return nil, nil }
	for name, tc := range map[string]struct {
		home func(*Home)
		want string
	}{
		"a device": {func(h *Home) { h.Device("Desk Plug") }, `"Desk Plug" isn't a device name`},
		"a room in a declaration": {
			func(h *Home) { h.Device("desk-plug").Room("Living Room") }, `"Living Room" isn't a room name`,
		},
		"a room":  {func(h *Home) { h.Room("Living Room").On.To(true) }, `"Living Room" isn't a room name`},
		"a group": {func(h *Home) { h.Group("Down Stairs", hallLamp) }, `"Down Stairs" isn't a group name`},
		"a case": {
			func(h *Home) {
				h.Decide(hallLamp.On).Case("Night_Time", false, ConditionFunc("night", func(Snapshot) bool { return true })).
					Default(true)
			},
			`"Night_Time" isn't a case name`,
		},
		"a flow":   {func(h *Home) { h.Flow("Good Morning", nothing).When(on...) }, `"Good Morning" isn't a flow name`},
		"a plugin": {func(h *Home) { h.Plugin("Zigbee.2", made) }, `"Zigbee.2" isn't a plugin name`},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := decide(tc.home).compile()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
			// The error suggests a name that follows the rule.
			if !strings.Contains(err.Error(), "such as \"") {
				t.Errorf("no suggestion in %v", err)
			}
		})
	}
}
