// Command home is a home that imports the simulated relays and does nothing else, so that the
// conformance suite can drive a plugin through the Go API.
package main

import (
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/framework"
	"github.com/harrymitchinson/oikade/sdk/plugin"
	"github.com/harrymitchinson/oikade/test/fixtures/simswitch/relays"
)

func main() {
	framework.Run(framework.Home{Plugins: []framework.Plugin{{
		Name: "simswitch",
		New: func(_ plugin.Environment, devices []contractv1.DeclaredDevice) (plugin.Handler, error) {
			return relays.New(devices), nil
		},
	}}})
}
