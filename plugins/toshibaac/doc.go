// Package toshibaac is the plugin for Toshiba Home AC Control, the cloud behind Toshiba's air
// conditioner app and its WiFi modules. A home imports it once per account:
//
//	home.Plugin("toshiba", toshibaac.New(toshibaac.Config{
//		Username: "someone@example.com", Password: oikade.Secret("toshiba"),
//	}))
//
// The modules listen for nothing on the LAN and only connect out to Toshiba's cloud (OIK-256), so
// the plugin speaks the app's protocol, as KaSroka/Toshiba-AC-control worked it out: it logs in over
// HTTPS, registers as a mobile device, lists the account's indoor units, then connects to Toshiba's
// Azure IoT Hub as that device, over MQTT 3.1.1 with the SAS token registering gave it. The hub
// calls the device's direct method smmobile with each unit's state as it changes, and with
// heartbeats that carry its room temperature. Every unit's state is also read over HTTP every 30
// minutes, in case the hub missed a change.
//
// Each unit is reported by an ID from its DeviceUniqueId, which stays the same when it's renamed,
// with on, mode, setpoint and temperature. The password is a secret, read at each login.
//
// A write is a CMD_FCU_TO_AC message to the hub, at QoS 1: a state in which only what changes is
// set. It returns once the hub has taken it, and refuses a setpoint that isn't whole degrees or a
// mode the unit's MeritFeature says it doesn't do. Units the home says share an outdoor unit, in
// Config.OutdoorUnits, can't be set running modes that clash, such as heating and cooling.
//
// While the cloud or the hub doesn't answer, every unit is offline and the plugin's status says why,
// and it tries again with backoff. A refused password is tried again once the secret changes, or
// hourly.
//
// The README beside this file says how to set it up.
package toshibaac
