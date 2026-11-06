// Command home is a small home for the host's tests. It imports only the public framework, and
// the tests build it as a module of its own to prove that's enough.
package main

import "github.com/harrymitchinson/oikade/framework"

const (
	on   = "switch.on"
	open = "sensor.contact-open"
)

func main() {
	framework.Run(framework.Home{
		Holds: []framework.Hold{
			{
				Name: "fan", Device: "fan", Capability: on, Value: true,
				When: func(s framework.State) bool {
					return s.Number("bathroom-sensor", "sensor.relative-humidity") > 70
				},
			},
			{
				Name: "leak-shutoff", Device: "valve", Capability: on, Value: false,
				Priority: framework.Safety, Latch: true,
				When: func(s framework.State) bool {
					return s.Bool("leak-sensor", open)
				},
			},
			{
				// With no arbiter to hold a claim, only the hold's saved state remembers this latch.
				Name: "leak-alarm", Device: "siren", Capability: on, Value: true, Latch: true,
				When: func(s framework.State) bool {
					return s.Bool("leak-sensor", open)
				},
			},
			{
				// A bug in a home: a condition that never returns once the sensor says so.
				Name: "stuck", Device: "trap-light", Capability: on, Value: true,
				When: func(s framework.State) bool {
					for s.Bool("trap", open) {
						continue
					}
					return false
				},
			},
		},
		Moments: []framework.Moment{
			{
				// A change here gets an answer, so a test knows when the process has caught up.
				Name: "probe", On: []framework.Change{{Device: "probe", Capability: open}}, Restart: true,
				Do: []framework.Action{framework.Set{Device: "probe-out", Capability: on, Value: true}},
			},
			{
				Name: "reset", On: []framework.Change{{Device: "reset", Capability: open}},
				Do: []framework.Action{framework.Release("leak-shutoff"), framework.Release("leak-alarm")},
			},
		},
	})
}
