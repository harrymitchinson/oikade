// Command home is a home that imports the simulated relays and does nothing else, so that the
// conformance suite can drive a plugin through the Go API.
package main

import (
	"oikade.dev"
	"oikade.dev/contract/v1"
	"oikade.dev/sdk/plugin"
	"oikade.dev/test/fixtures/simswitch/relays"
)

func main() {
	factory := func(_ plugin.Environment, devices []contract.DeclaredDevice) (plugin.Handler, error) {
		return relays.New(devices), nil
	}
	home := oikade.NewHome(oikade.Options{})
	home.Plugin("simswitch", factory)
	home.Run()
}
