// Package plugin is the Go SDK for Oikade plugins. It implements the oikade/v1 contract
// (docs/plugin-contract.md), including its hard parts: the Last Will, re-announcing everything
// after a reconnect, dispatching /set and going offline cleanly on SIGTERM.
//
// A plugin implements Handler and calls Run from main. Run reads the environment and devices.json,
// builds the handler from the declared devices, and serves until it's stopped:
//
//	func main() {
//		plugin.Run(func(_ plugin.Environment, devices []contractv1.DeclaredDevice) (plugin.Handler, error) {
//			return newSwitches(devices), nil // simulated relays, one per declared device
//		})
//	}
//
// A plugin that a home imports as a framework.Plugin needs no main of its own: it gives the home a
// Factory, and the host runs it in a process of its own through RunHosted. A Handler is the same code
// either way. Importing a plugin means trusting its code: it runs as you.
//
// Handler.Serve reports the devices with SetDevices, then each device's SetAvailability and
// SetState, or ReportState for a device that can't be asked for its values, and follows the devices
// until its context ends. A plugin that can list the devices in
// its app also reports them with SetDiscovered, under IDs that survive restarts. Handler.Set
// applies one command and reports the result with SetState or UpdateState. Set calls for one device never overlap, but
// they run on their own goroutines alongside Serve, so state both use needs a lock.
//
// Everything a plugin publishes is checked first. SetState refuses a device that isn't found, a
// capability the device hasn't announced, a value the capability doesn't accept, and a state
// missing an announced capability. The mistake is then the plugin's error, not a message Oikade
// rejects.
//
// Logs are JSON on stderr. The SDK logs "Plugin came online", "Could not announce plugin",
// "Ignored set command", "Could not apply set command", "Could not start plugin", "Could not
// serve plugin" and "Could not close plugin". It never logs secrets: ReadSecret's values are the plugin's to protect.
//
// test/fixtures/simswitch in the Oikade repository is a complete plugin.
package plugin
