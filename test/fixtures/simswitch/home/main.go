// Command home is a home that imports the simulated relays and does nothing else, so that the
// conformance suite can drive a plugin through the Go API.
package main

import (
	"github.com/harrymitchinson/oikade"
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/sdk/plugin"
	"github.com/harrymitchinson/oikade/test/fixtures/simswitch/relays"
)

func main() {
	factory := func(_ plugin.Environment, devices []contractv1.DeclaredDevice) (plugin.Handler, error) {
		return relays.New(devices), nil
	}
	oikade.Run(oikade.NewHome(oikade.Options{}, oikade.NewPlugin("simswitch", factory)))
}
