// Package toshibaac is the plugin for Toshiba Home AC Control, the cloud behind Toshiba's air
// conditioner app and its WiFi modules. A home imports it once per account:
//
//	home.Plugin("toshiba", toshibaac.New(toshibaac.Config{
//		Username: oikade.Var("TOSHIBA_USER"), Password: oikade.Var("TOSHIBA_PASSWORD"),
//	}))
//
// The modules listen for nothing on the LAN and only connect out, so the plugin speaks the app's
// cloud protocol, as KaSroka/Toshiba-AC-control documents it, with the headers api.go says the
// cloud requires. A session logs in over HTTPS and registers as one mobile device per account
// (clientID), which gives an Azure IoT Hub SAS token. It lists the account's units, then connects
// to the hub the token names, as that device, over MQTT 3.1.1 (mqttConn). The hub calls the
// device's direct method smmobile with CMD_FCU_FROM_AC, a unit's state as hex in which a field it
// isn't reporting is a none value, so fcuState updates only the fields a report has, and with
// CMD_HEARTBEAT, the unit's room temperature. Each call is answered with status 0. The session also
// reads every unit's state over HTTP every reload interval, beside the hub so a slow read doesn't
// hold up its calls. A read that fails is only logged, unless the access token has run out, which
// ends the session.
//
// A unit's ID is its DeviceUniqueId, which stays the same when it's renamed: trimmed and in lower
// case, with any character an ID can't hold as '-', no leading '_', '.' or '-', and cut to 63.
//
// A write is CMD_FCU_TO_AC, a device-to-cloud message at QoS 1 with a state in which only what
// changes is set (command), and Set returns on the hub's PUBACK. It's worked out from the unit's
// state with the writes sent since its last report (intent, recorded before the write is sent and
// counting for timing's intent at most), so a mode and a setpoint sent together see the new mode.
// Set refuses a setpoint that isn't whole degrees and a mode bits 6 and 7 of the unit's
// MeritFeature rule out. The cloud doesn't say which units share an outdoor unit, so
// Config.OutdoorUnits does, and a write that would run one in a mode that clashes with another one
// running is refused (clashes).
//
// A session whose access token (a 401) or SAS token (renew before its expiry) runs out starts again
// at once with its units online, unless it ended as it began, and only a session that lasted resets
// the backoff. A login refused waits an hour, as too many failed logins could lock the account and
// a new password comes with a deploy, which restarts the plugin. A limit, a 403 or a 429, waits as
// long as the cloud asks, doubling while limits follow one another, between retry and limited, and
// limited when it doesn't say (limitWait). Anything else backs off from retry to retryMax. Each
// problem is the plugin's status error. While there's no session every unit is offline, the
// discovered list is nil and a write fails at once.
//
// The README beside this file is the plugin's user docs.
package toshibaac
