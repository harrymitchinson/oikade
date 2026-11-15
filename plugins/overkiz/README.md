# Overkiz plugin

This plugin connects devices on Overkiz, the platform behind Somfy TaHoma and Atlantic's, Thermor's and Sauter's Cozytouch. It's a plugin your home imports ([OIK-17](https://linear.app/oikade/issue/OIK-17)), once for each hub or account. It talks to a TaHoma hub's local API, so your blinds keep working while the internet is down, and to Cozytouch's cloud, which is all those heaters have. Every device it serves appears in `oikade devices`, under its ID.

```go
framework.Run(framework.NewHome(framework.Options{},
	framework.NewPlugin("tahoma", overkiz.New(overkiz.Local{
		Address:     "192.168.1.40:8443", // the hub, or gateway-1234-5678-9012.local:8443
		TokenSecret: "tahoma-token",
	})),
	framework.NewPlugin("cozytouch", overkiz.New(overkiz.Cozytouch{
		Brand: overkiz.Thermor, Username: "someone@example.com", PasswordSecret: "cozytouch",
	})),
	// ...
))
```

## Setting up a TaHoma hub

The local API needs a TaHoma Switch, or another TaHoma with Developer Mode. The first-generation square TaHoma and the Connectivity Kit don't have it.

1. **Turn on Developer Mode** in the TaHoma By Somfy app: under "Configure the installation", then "Access the parameters", tap the hub's PIN (such as `1234-5678-9012`) seven times.
2. **Make a token** from the Developer Mode menu, as [Somfy's Developer Mode guide](https://github.com/Somfy-Developer/Somfy-TaHoma-Developer-Mode) says. The app shows it only once, so copy it then.
3. **Give the token to Oikade:** run `oikade secret set tahoma-token`, and paste it. The plugin reads it at each login, so a new token needs no deploy.

Until the token is set, or while the hub refuses it, `oikade status` says so, and the plugin waits for the secret to change rather than trying the same token again. It tries a refused token once an hour.

The hub's certificate is signed by Overkiz's own authority for its `gateway-<pin>.local` name, which the system doesn't trust and an IP address doesn't match, so the plugin doesn't check it. Keep the hub on a network you trust.

## Setting up a Cozytouch account

`Brand` is the app your account is in: `overkiz.Atlantic`, `overkiz.Thermor` or `overkiz.Sauter`. `Username` is the account's email address, and its password is a secret: run `oikade secret set cozytouch`, and paste it. It's read at each login, so a new password needs no deploy.

A refused password shows in `oikade status`, and the plugin waits for the secret to change, or an hour, rather than trying again, since too many failed logins can lock the account. A session the cloud ends is logged in again by itself.

**The Cozytouch API isn't published.** The plugin logs in as Atlantic's app does, with its client ID, as [pyoverkiz](https://github.com/iMicknl/python-overkiz-api) does, and Atlantic can change that. If it does, the plugin's status says the login failed.

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

Any other kind of device isn't reported yet, and the host's log names its widget once. **RTS blinds aren't reported:** they don't say where they are, and Oikade can't show a guess.

The plugin fetches events every 5 seconds from a hub and every 30 from the cloud, which limits how often it's asked, and every 2 while a command runs, and reads the whole setup again only when it starts, when a device is added, removed or changed in the app, when a command fails, and when the hub has forgotten its listener. A device you disable in the app is offline until the setup is next read. While the hub or cloud doesn't answer, every device is offline, `oikade status` says why, and the plugin tries again: from 5 seconds up to a minute apart for a hub, and 30 seconds up to 5 minutes for the cloud. Told there were too many requests, it waits a minute for a hub, or 10 for the cloud, and a write meanwhile fails at once.
