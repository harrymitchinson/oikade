// Command simswitch is a test fixture: an Oikade plugin that serves simulated relays, one per
// declared device. make test-conformance and the oikade-conformance tests drive it through the
// oikade/v1 contract, so SDK changes are tested end to end in the same PR. Each relay reports on, as
// docs/plugin-contract.md#relay-like-devices says, and one whose ID ends in .misconfigured is
// reported as a device error.
//
// The plugin itself is in relays, and home imports it through the Go API. It isn't published.
package main

import (
	"oikade.dev/contract/v1"
	"oikade.dev/sdk/plugin"
	"oikade.dev/test/fixtures/simswitch/relays"
)

func main() {
	plugin.Run(func(_ plugin.Environment, devices []contract.DeclaredDevice) (plugin.Handler, error) {
		return relays.New(devices), nil
	})
}
