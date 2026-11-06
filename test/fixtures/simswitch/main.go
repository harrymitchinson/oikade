// Command simswitch is a test fixture: an Oikade plugin that serves simulated relays, one per
// declared device. make test-conformance and the oikade-conformance tests drive it through the
// oikade/v1 contract, so SDK changes are tested end to end in the same PR. It follows the relay
// convention (docs/plugin-contract.md#relay-like-devices): switch.on by default, or light.on or
// outlet.on from parameters.capability. Any other value is reported as a device error.
//
// The plugin itself is in relays, and home imports it through the Go API. It isn't published.
// Plugins live in their own repos (oikade-plugin-<name>).
package main

import (
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/sdk/plugin"
	"github.com/harrymitchinson/oikade/test/fixtures/simswitch/relays"
)

func main() {
	plugin.Run(func(_ plugin.Environment, devices []contractv1.DeclaredDevice) (plugin.Handler, error) {
		return relays.New(devices), nil
	})
}
