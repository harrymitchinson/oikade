# Overkiz plugin

This plugin connects devices on Overkiz, the platform behind Somfy TaHoma and Atlantic's, Thermor's and Sauter's Cozytouch, and on Magellan, Atlantic's own cloud behind the same Cozytouch app. It's a plugin your home imports (OIK-17), once for each hub or account. It talks to a TaHoma hub's local API, so your blinds keep working while the internet is down, and to Cozytouch's clouds, which are all those heaters have. Every device it serves appears in `oikade devices`, under its hardware's ref, `hardware/<plugin>/<ID>` by the name your home imports it by.

A TaHoma hub puts its devices in the Home app itself, so on Oikade's Matter bridge too they'd show twice. `HideFromMatter()` on the import keeps them off it, and `ShowOnMatter(names...)` names any TaHoma doesn't share ([Hiding a plugin's devices from Matter](../../docs/devices.md#hiding-a-plugins-devices-from-matter)).

```go
home.Plugin("tahoma", overkiz.New(overkiz.Local{
	Address: "192.168.1.40:8443", // the hub, or gateway-1234-5678-9012.local:8443
	PIN:     "1234-5678-9012",    // the hub's PIN, which an IP address doesn't give
	Token:   oikade.Secret("tahoma-token"),
}))
home.Plugin("cozytouch", overkiz.New(overkiz.Cozytouch{
	Brand: overkiz.Thermor, Username: "someone@example.com", Password: oikade.Secret("cozytouch"),
}))
```

## Setting up a TaHoma hub

The local API needs a TaHoma Switch, or another TaHoma with Developer Mode. The first-generation square TaHoma and the Connectivity Kit don't have it.

1. **Turn on Developer Mode** in the TaHoma By Somfy app: under "Configure the installation", then "Access the parameters", tap the hub's PIN (such as `1234-5678-9012`) seven times.
2. **Make a token** from the Developer Mode menu, as [Somfy's Developer Mode guide](https://github.com/Somfy-Developer/Somfy-TaHoma-Developer-Mode) says. The app shows it only once, so copy it then.
3. **Give the token to Oikade:** run `oikade secret set tahoma-token`, and paste it. The plugin reads it at each login, so a new token needs no deploy.

Until the token is set, or while the hub refuses it, `oikade status` says so, and the plugin waits for the secret to change rather than trying the same token again. It tries a refused token once an hour.

**The hub's certificate is checked** (OIK-292), so nothing else on the network can pose as the hub and take its token. Overkiz signs each hub's certificate with its own root authority, which the plugin carries, for the hub's name, `gateway-<pin>.local`. The plugin checks that name against the PIN in `Address`, or in `PIN` for any other `Address`, such as an IP address. Without a PIN it can only check that Overkiz signed the certificate, so any TaHoma hub's would pass, and it logs a warning saying so. A certificate it refuses shows in `oikade status` with the names it gives and what to check, and the plugin tries again with backoff. If your hub's real certificate is refused, `SkipCertificateCheck: true` turns the check off, and anything on the network that poses as the hub then gets its token.

## Setting up a Cozytouch account

`Brand` is the app your account is in: `overkiz.Atlantic`, `overkiz.Thermor` or `overkiz.Sauter`. `Username` is the account's email address, and its password is a secret: run `oikade secret set cozytouch`, and paste it. It's read at each login, so a new password needs no deploy.

**Your account's devices can be on either of Atlantic's clouds, or both:** Overkiz, or Magellan, where newer WiFi devices such as a Thermor Riva 5 are (OIK-329). You don't need to know which. The plugin logs in once and reads both. A cloud that has no account for your login, as Overkiz answers `No such user account` for an account on Magellan, isn't a problem: the plugin logs it once, and asks it again hourly.

A refused password shows in `oikade status`, and the plugin waits for the secret to change, or an hour, rather than trying again, since too many failed logins can lock the account. A session the cloud ends is logged in again by itself.

**The Cozytouch APIs aren't published.** The plugin logs in as Atlantic's app does, with its client ID, as [pyoverkiz](https://github.com/iMicknl/python-overkiz-api) does, and speaks Magellan as the [Homey app](https://github.com/NicolasYDDER/homey-cozytouch) does. Atlantic can change either. If it does, the plugin's status says the login failed.

**A Safety claim on a cloud device can't act while the cloud is down.** A heater keeps its own controls, but Oikade can't turn it off until the cloud is back.

## What it reports

Each device's ID comes from its Overkiz device URL, `<protocol>://<hub>/<address>`, as `<protocol>.<address>`, such as `io.11111111`. It leaves out the hub, so a replaced hub keeps your devices, their rooms and their Home app automations. A device's sub-devices, such as a sensor inside it, are part of it.

| Widget | Somfy devices | Capabilities |
|---|---|---|
| `PositionableRollerShutter`, `PositionableRollerShutterWithLowSpeedManagement` | io roller shutters | `position` |
| `PositionableScreen` | io screens | `position` |
| `AtlanticElectricalTowelDryer` | Atlantic, Thermor and Sauter towel radiators | `on`, `setpoint`, `temperature` |

`position` is how far closed it is, 0 open to 100 closed. A write returns once the hub has taken the command, and the shutter's own reports show it moving.

A towel radiator is a thermostat in the Home app. `on` false is its standby, and `on` true from standby is its manual mode, which holds `setpoint`. In its own schedule, its auto mode or any mode the plugin doesn't know, it's on, and stays in that mode: `setpoint` is then the temperature it's aiming for, and a write is the override those modes take. Its boost and drying modes aren't served, and in them it reports the setpoint it's really aiming for. `temperature` is its room sensor, at `#7`. The mapping is Home Assistant's, and is yet to be checked against a real radiator.

### On Magellan

A Magellan device's ID is `magellan.<deviceId>`, such as `magellan.28186340`, from the device's ID in the app, and its suggested name is its room's, such as "Salle de bain".

| Models | Devices | Capabilities |
|---|---|---|
| 1381, 1382, 1386, 1543, 1546, 1547, 1551, 1560, 1622 | Atlantic, Thermor and Sauter towel radiators, such as the Thermor Riva 5 | `on`, `setpoint`, `temperature` |

A towel radiator on Magellan means what one on Overkiz does. `on` false is off, and `on` true from off is its manual mode. In its weekly programme it's on, and stays in it: `setpoint` is the temperature it's aiming for, and a write is an override until the programme's next step. A setpoint outside the radiator's own range, 7 to 28 °C on a Riva 5, is refused. Its boost and drying modes aren't served. `temperature` is its room sensor. The mapping has been checked on a Riva 5: off and on, and a setpoint in its programme.

Magellan has no events, so the plugin reads the account's whole setup once a minute, one request, and once a write is done, which takes a few seconds. A write returns once the cloud has taken it. Magellan refuses a write to a device while its last one runs, so a write that comes meanwhile, such as a setpoint slider moved and moved back, waits for it, and a newer write to the same capability replaces one still waiting: the device ends where you left it (OIK-339). A write Magellan refuses anyway, as while the Cozytouch app's own runs, is logged as `Magellan execution failed`, with the device and what it set. While Magellan doesn't answer, its devices are offline and `oikade status` says why, while Overkiz's devices carry on.

Any other kind of device isn't reported yet, and the host's log names its widget, or Magellan's model, once. **RTS blinds aren't reported:** they don't say where they are, and Oikade can't show a guess.

The plugin fetches events every 5 seconds from a hub and every 30 from the cloud, which limits how often it's asked, and every 2 while a command runs, and reads the whole setup again only when it starts, when a device is added, removed or changed in the app, when a command fails, and when the hub has forgotten its listener. A device you disable in the app is offline until the setup is next read. While the hub or cloud doesn't answer, every device is offline, `oikade status` says why, and the plugin tries again: from 5 seconds up to a minute apart for a hub, and 30 seconds up to 5 minutes for the cloud. Told there were too many requests, it waits a minute for a hub, or 10 for the cloud, and a write meanwhile fails at once.
