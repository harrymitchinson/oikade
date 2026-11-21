# Overkiz plugin

This plugin serves devices on Overkiz, the platform behind Somfy's TaHoma hubs and Atlantic's, Thermor's and Sauter's Cozytouch app. It also serves Magellan, Atlantic's own cloud behind the same app. Your home imports it once for each hub or account.

It reports roller shutters, screens and towel radiators. Each appears in `oikade list devices` as `hardware/<plugin>/<ID>`, where `<plugin>` is the name your home imports it by.

## Setting up a TaHoma hub

The plugin talks to the hub on your network, so your blinds keep working while the internet is down. It needs a TaHoma Switch, or another TaHoma with Developer Mode. The first-generation square TaHoma and the Connectivity Kit don't have it.

1. **Turn on Developer Mode** in the TaHoma By Somfy app: under "Configure the installation", then "Access the parameters", tap the hub's PIN (such as `1234-5678-9012`) seven times.
2. **Make a token** from the Developer Mode menu, as [Somfy's Developer Mode guide](https://github.com/Somfy-Developer/Somfy-TaHoma-Developer-Mode) says. The app shows it only once, so copy it then.
3. **Import the plugin** with `overkiz.Local`, and put its values in your home's `.env` ([Your values](../../docs/running-a-home.md#your-values)):

```go
home.Plugin("tahoma", overkiz.New(overkiz.Local{
	Address: oikade.Var("TAHOMA_ADDRESS"), PIN: oikade.Var("TAHOMA_PIN"), Token: oikade.Var("TAHOMA_TOKEN"),
}))
```

```sh
# .env
TAHOMA_ADDRESS=192.168.1.40:8443   # or gateway-1234-5678-9012.local:8443
TAHOMA_PIN=1234-5678-9012
TAHOMA_TOKEN=…
```

4. **Deploy.** `oikade deploy` won't go ahead while a value is missing from `.env`.

| `overkiz.Local` field | What it is |
|---|---|
| `Address` | The hub's host and port: its IP address, or its own name, `gateway-<pin>.local`, which the plugin looks up over mDNS. |
| `Token` | The Developer Mode token. |
| `PIN` | The hub's PIN, which its certificate is checked against. Set it when `Address` isn't `gateway-<pin>.local`, which gives it already. |
| `SkipCertificateCheck` | `true` trusts whatever answers at `Address`. Leave it off unless the check refuses your hub's real certificate. |

**The hub's certificate is checked,** so nothing else on your network can pose as the hub and take its token. Overkiz signs each hub's certificate for its name, `gateway-<pin>.local`, and the plugin carries Overkiz's root certificate and checks that name against the PIN. Without a PIN it can only check that Overkiz signed the certificate, so any TaHoma hub's would pass, and it logs a warning saying so. A certificate it refuses shows in `oikade status`, with the names it gives and what to check.

**A TaHoma hub puts its devices in the Home app itself,** so on Oikade's Matter bridge too they'd show twice. `HideFromMatter()` keeps them off the bridge, and `ShowOnMatter(names...)` names any TaHoma doesn't share ([Hiding a plugin's devices from Matter](../../docs/devices.md#hiding-a-plugins-devices-from-matter)).

## Setting up a Cozytouch account

Atlantic's, Thermor's and Sauter's heaters have no local API, so the plugin talks to their clouds with your Cozytouch login.

```go
home.Plugin("cozytouch", overkiz.New(overkiz.Cozytouch{
	Brand: overkiz.Thermor, Username: oikade.Var("COZYTOUCH_USER"), Password: oikade.Var("COZYTOUCH_PASSWORD"),
}))
```

```sh
# .env
COZYTOUCH_USER=someone@example.com
COZYTOUCH_PASSWORD=…
```

| `overkiz.Cozytouch` field | What it is |
|---|---|
| `Brand` | The app your account is in: `overkiz.Atlantic`, `overkiz.Thermor` or `overkiz.Sauter`. |
| `Username` | The account's email address. |
| `Password` | The account's password. |

**Your devices can be on either of Atlantic's clouds, or both:** Overkiz, or Magellan, where newer WiFi devices such as a Thermor Riva 5 are. You don't need to know which. The plugin logs in once and reads both. A cloud with no account for your login isn't a problem: the plugin logs that once and asks again hourly.

**The Cozytouch APIs aren't published.** The plugin logs in as Atlantic's app does, as [pyoverkiz](https://github.com/iMicknl/python-overkiz-api) does, and speaks Magellan as the [Homey app](https://github.com/NicolasYDDER/homey-cozytouch) does. If Atlantic changes either, the plugin's status says the login failed.

## What it reports

No device's serial number or firmware version is reported, since the setups seen so far carry none.

### On Overkiz

A device's ID comes from its Overkiz device URL, `<protocol>://<hub>/<address>`, as `<protocol>.<address>`, such as `io.11111111`. It leaves out the hub, so a replaced hub keeps your devices, their rooms and their Home app automations. A device's sub-devices, such as a sensor inside it, are part of it. Its manufacturer is its `core:Manufacturer` attribute, or on Cozytouch the account's brand, its model its `controllableName`, and its suggested name its label in the app.

| Widget | Devices | Capabilities |
|---|---|---|
| `PositionableRollerShutter`, `PositionableRollerShutterWithLowSpeedManagement` | Somfy io roller shutters | `position` |
| `PositionableScreen` | Somfy io screens | `position` |
| `AtlanticElectricalTowelDryer` | Atlantic, Thermor and Sauter towel radiators | `on`, `setpoint`, `temperature` |

- **`position`** is how far closed it is, from 0 open to 100 closed. A shutter that can't say where it is, such as one not yet calibrated, reports no position.
- **A towel radiator** is a thermostat in the Home app. `on` false is its standby, and `on` true from standby is its manual mode, which holds `setpoint`. In its own schedule, its auto mode or a mode the plugin doesn't know, it's on and stays in that mode: `setpoint` is the temperature it's aiming for, and a write is the override that mode takes. `temperature` is its room sensor, the sub-device at `#7`. This mapping is Home Assistant's, and hasn't been checked against a real radiator.

### On Magellan

A device's ID is `magellan.<deviceId>`, such as `magellan.28186340`, from its ID in the app. Its manufacturer is the account's brand, its model its name in the app, and its suggested name its room's, such as "Salle de bain".

| Models | Devices | Capabilities |
|---|---|---|
| 1381, 1382, 1386, 1543, 1546, 1547, 1551, 1560, 1622 | Atlantic, Thermor and Sauter towel radiators, such as the Thermor Riva 5 | `on`, `setpoint`, `temperature` |

A towel radiator on Magellan means what one on Overkiz does. `on` false is off, and `on` true from off is its manual mode. In its weekly programme it's on and stays in it: `setpoint` is the temperature it's aiming for, and a write overrides it until the programme's next step. A setpoint outside the radiator's own range, 7 to 28 °C on a Riva 5, is refused. `temperature` is its room sensor. This mapping has been checked on a Riva 5: off and on, and a setpoint in its programme.

## Writes

A write returns once the hub or cloud has taken the command, before the device has acted. The device's own reports then show it moving, or its setpoint changing. A value outside what the capability takes, such as a `position` of 150, is refused rather than sent.

Magellan refuses a write to a device while its last one runs, so a write that comes meanwhile, such as a setpoint slider moved and moved back, waits for it. A newer write to the same capability replaces one still waiting, so the device ends where you left it. A write Magellan refuses anyway, as while the Cozytouch app's own runs, is logged as `Magellan execution failed`, with the device and what it set.

## Keeping up, and when things go wrong

| | TaHoma hub | Cozytouch cloud |
|---|---|---|
| Fetches events | every 5 s | every 30 s |
| While a command runs | every 2 s | every 2 s |
| Reads Magellan's whole setup | | every minute, and once a write is done |
| Tries again after no answer | from 5 s, doubling up to a minute | from 30 s, doubling up to 5 minutes |
| Waits after "too many requests" | a minute | 10 minutes |
| Tries a refused token or password again | after an hour | after an hour |

The plugin reads a hub's or Overkiz's whole setup again only when it logs in, when a device is added, removed or changed in the app, when a command fails, and when the hub has forgotten its listener. A device you disable in the app goes offline, and comes back once you enable it and the setup is next read.

While a hub or cloud doesn't answer, its devices are offline, `oikade status` says why, and a write fails at once. On a Cozytouch account, one cloud down leaves the other's devices working. A refused token or password shows in `oikade status`, and is tried again only hourly, since too many failed logins can lock the account. A new one goes up with `oikade deploy`, which restarts the plugin, so it's tried at once. A cloud session that lasted 5 minutes or more and that the cloud then ends is logged in again at once, with its devices online meanwhile. One the cloud ends sooner waits, from 30 seconds, doubling up to 5 minutes.

## What it can't do

- **Any other kind of device isn't reported.** The host's log names its widget, or its Magellan model, once.
- **RTS blinds aren't reported:** they don't say where they are, and Oikade can't show a guess.
- **A towel radiator's boost and drying modes aren't served.** In one, it reports the setpoint it's really aiming for.
- **A Safety claim on a cloud device can't act while the cloud is down.** A heater keeps its own controls, but Oikade can't turn it off until the cloud is back.
