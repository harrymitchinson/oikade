# Overkiz plugin

This plugin connects devices on Overkiz, the platform behind Somfy TaHoma and others. It's a plugin your home imports ([OIK-17](https://linear.app/oikade/issue/OIK-17)). It talks to a TaHoma hub's local API, so your blinds keep working while the internet is down, and every device the hub lists appears in `oikade devices`, under its ID.

```go
framework.Run(framework.NewHome(framework.Options{},
	framework.NewPlugin("tahoma", overkiz.New(overkiz.Local{
		Address:     "192.168.1.40:8443", // the hub, or gateway-1234-5678-9012.local:8443
		TokenSecret: "tahoma-token",
	})),
	// ...
))
```

Cozytouch accounts, such as Atlantic's and Thermor's heaters, will connect over the cloud. That's the next step of OIK-17.

## Setting up a TaHoma hub

The local API needs a TaHoma Switch, or another TaHoma with Developer Mode. The first-generation square TaHoma and the Connectivity Kit don't have it.

1. **Turn on Developer Mode** in the TaHoma By Somfy app: under "Configure the installation", then "Access the parameters", tap the hub's PIN (such as `1234-5678-9012`) seven times.
2. **Make a token** from the Developer Mode menu, as [Somfy's Developer Mode guide](https://github.com/Somfy-Developer/Somfy-TaHoma-Developer-Mode) says. The app shows it only once, so copy it then.
3. **Give the token to Oikade:** run `oikade secret set tahoma-token`, and paste it. The plugin reads it at each login, so a new token needs no deploy.

Until the token is set, or while the hub refuses it, `oikade status` says so, and the plugin waits for the secret to change rather than trying the same token again. It tries a refused token once an hour.

The hub's certificate is signed by Overkiz's own authority for its `gateway-<pin>.local` name, which the system doesn't trust and an IP address doesn't match, so the plugin doesn't check it. Keep the hub on a network you trust.

## What it reports

Each device's ID comes from its Overkiz device URL, `<protocol>://<hub>/<address>`, as `<protocol>.<address>`, such as `io.11111111`. It leaves out the hub, so a replaced hub keeps your devices, their rooms and their Home app automations. A device's sub-devices, such as a sensor inside it, are part of it.

| Widget | Somfy devices | Capabilities |
|---|---|---|
| `PositionableRollerShutter`, `PositionableRollerShutterWithLowSpeedManagement` | io roller shutters | `position` |
| `PositionableScreen` | io screens | `position` |

`position` is how far closed it is, 0 open to 100 closed. A write returns once the hub has taken the command, and the shutter's own reports show it moving.

Any other kind of device isn't reported yet, and the host's log names its widget once. **RTS blinds aren't reported:** they don't say where they are, and Oikade can't show a guess.

The plugin fetches the hub's events every 5 seconds, and every 2 while a command runs, and reads the whole setup again only when it starts, when a device is added, removed or changed in the app, when a command fails, and when the hub has forgotten its listener. A device you disable in the app is offline until the setup is next read. While the hub doesn't answer, every device is offline, `oikade status` says why, and the plugin tries again, from 5 seconds up to a minute apart.
