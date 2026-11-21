# Toshiba Home AC Control plugin

This plugin serves Toshiba air conditioners that have a WiFi module, through Toshiba Home AC Control, the cloud behind Toshiba's app. Your home imports it once for each account.

Every indoor unit in the account appears in `oikade list devices` as `hardware/<plugin>/<ID>`, where `<plugin>` is the name your home imports it by, and on the Matter bridge as a Room Air Conditioner.

## Setting it up

1. **Add your units in Toshiba's Home AC Control app**, if they aren't there already. The plugin sees what the app's account sees.
2. **Import the plugin** with the app's account, and put its username and password in your home's `.env` ([Your values](../../docs/running-a-home.md#your-values)):

```go
home.Plugin("toshiba", toshibaac.New(toshibaac.Config{
	Username: oikade.Var("TOSHIBA_USER"), Password: oikade.Var("TOSHIBA_PASSWORD"),
}))
```

```sh
# .env
TOSHIBA_USER=someone@example.com
TOSHIBA_PASSWORD=…
```

3. **Deploy.** `oikade deploy` won't go ahead while a value is missing from `.env`.

| `toshibaac.Config` field | What it is |
|---|---|
| `Username` | The app account's username, usually your email address. |
| `Password` | The app account's password. |
| `OutdoorUnits` | The indoor units that share an outdoor unit, a list for each outdoor unit. See [Units that share an outdoor unit](#units-that-share-an-outdoor-unit). |

**The units work only through Toshiba's cloud.** Their WiFi modules listen for nothing on your network, so the plugin can't reach them locally. While the internet or Toshiba's cloud is down, every unit is offline, `oikade status` says why, and a write fails at once. A unit keeps its own remote and its own timers.

**Toshiba's API isn't published.** The plugin logs in as Toshiba's app does, as [KaSroka/Toshiba-AC-control](https://github.com/KaSroka/Toshiba-AC-control) worked it out, with the headers its maintained fork, [vmvelev/Toshiba-AC-control](https://github.com/vmvelev/Toshiba-AC-control), found the cloud wants. If Toshiba changes it, the plugin's status says the login failed. The plugin registers with the account as a phone would, under one name for the account, so it doesn't fill the account with devices.

## What it reports

Each unit's ID is its `DeviceUniqueId` in Toshiba's API, so renaming it in the app keeps its rooms, its history and its Home app automations. The ID is in lower case, with any character an ID can't hold as `-`, no leading `_`, `.` or `-`, and at most 63 characters. Its name in the app is its suggested name, and its manufacturer is Toshiba. Its model, serial number and firmware version aren't reported, since what Toshiba's API says of them hasn't been seen from a real unit.

| Capability | From the unit |
|---|---|
| `on` | Its power. |
| `mode` | `auto`, `heat`, `cool`, `dry` or `fan`, kept while it's off. |
| `setpoint` | The temperature it aims for, in whole degrees Celsius. In 8 °C heating it's the real 8 °C, not the 24 the unit reports. |
| `temperature` | Its room sensor. |

Toshiba's cloud passes on each change to a unit as it happens, and the unit's room temperature with its heartbeats. The plugin also reads every unit's state every 30 minutes, in case a change was missed, and reads the account's units again whenever it logs in: when it starts, when it reconnects, and each time its session's tokens run out.

## Changing a unit

`on`, `mode` and `setpoint` can be written, from your code or the Home app. A write returns once Toshiba's cloud has taken it, before the unit has changed: the unit's own report then shows what it did.

- **Writing `on` true** clears a unit's self-cleaning, which would keep it off, as the app does.
- **Changing `mode`** turns off a feature the new mode doesn't allow, such as 8 °C heating when it starts cooling.
- **A `setpoint` that isn't a whole degree fails,** rather than being rounded. Which setpoints a unit takes isn't known, so one it doesn't take is sent, and its report then shows the setpoint it kept.
- **A `mode` the unit doesn't do fails.** Which modes it does comes from what Toshiba's cloud says of it: a heater only heats, and an air conditioner without a heat pump does everything but heat.

### Units that share an outdoor unit

Indoor units on one outdoor unit, a multi-split, can't heat and cool at once. Toshiba's cloud takes such a write anyway, and the unit then reports the mode it was given without running it. The cloud doesn't say which units share an outdoor unit, so tell the plugin, in `OutdoorUnits`. Name each unit by its ID, the last part of its `hardware/…` ref in `oikade list devices` (`id` in `--json`), or by its `DeviceUniqueId` as the app's API writes it:

```go
home.Plugin("toshiba", toshibaac.New(toshibaac.Config{
	Username: oikade.Var("TOSHIBA_USER"), Password: oikade.Var("TOSHIBA_PASSWORD"),
	OutdoorUnits: [][]string{{"a1b2c3d4e5f6", "0a0b0c0d0e0f"}},
}))
```

Each list has at least two units, and a unit is in one list at most. An ID in `OutdoorUnits` that isn't on the account is logged at each login.

A write that would run a unit in a mode that clashes with another one running then fails, and says which one:

- Heating clashes with cooling and dry.
- `auto` clashes with heating, cooling and dry, since it may pick either.
- Fan runs beside any mode.
- Turning a unit off, changing its setpoint, and changing the mode of a unit that's off are never refused, so the Home app can set a mode before turning a unit on.
- A unit whose mode isn't known yet can't be turned on beside a running one, since it would run in whatever mode it last had.

The check counts the writes sent to each unit, from before they're sent, until the unit reports, so two writes at once to units on one outdoor unit can't both pass. A write the unit never reports, as when it has lost its WiFi, stops counting after a minute, and its last report is checked instead. A refused write from the Home app shows on its tile until the unit's next report puts it back.

## When things go wrong

- **A refused password** shows in `oikade status`, and the plugin waits an hour before trying it again, since too many failed logins could lock the account. A new password goes up with `oikade deploy`, which restarts the plugin, so it's tried at once.
- **While the cloud doesn't answer,** the plugin tries again from 30 seconds, doubling up to 5 minutes apart.
- **Too many requests:** Toshiba's cloud refuses logins that come too often, and a deploy logs the plugin in again, since it restarts it. The cloud has refused the eighth login in two minutes, with deploys a few seconds apart. Told there were too many requests, the plugin waits as long as the cloud asks, which has been a minute, or 10 minutes when the cloud doesn't say. If it's still refused, the plugin waits twice as long each time, up to 10 minutes, until it connects. A deploy meanwhile starts it again, and it logs in at once. While it waits, every unit is offline, and `oikade status` says how long the wait is.

## What it can't do

- **Fan speed, swing and Toshiba's other features** aren't reported or set.
- **A mode the plugin doesn't know,** such as one a later unit adds, isn't reported, and the host's log names it once.
- **A Safety claim on a unit can't act while the cloud is down.** The unit keeps its own controls, but Oikade can't turn it off until the cloud is back.
