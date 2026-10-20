// Command example-switch is an Oikade plugin that serves simulated relays, one per declared
// device. CI's conformance suite runs against it.
package main

import (
	contractv1 "github.com/harrymitchinson/oikade/contract/v1"
	"github.com/harrymitchinson/oikade/sdk/plugin"
)

func main() {
	plugin.Run(func(_ plugin.Environment, devices []contractv1.DeclaredDevice) (plugin.Handler, error) {
		return newSwitches(devices), nil
	})
}
