# Toshiba Home AC Control plugin

This plugin connects Toshiba air conditioners with a WiFi module, through Toshiba Home AC Control, the cloud behind Toshiba's app ([OIK-256](https://linear.app/oikade/issue/OIK-256)). It's a plugin your home imports, once for each account. Every indoor unit in the account appears in `oikade devices`, under its ID, and on the Matter bridge as a Room Air Conditioner.

```go
home.Plugin("toshiba", toshibaac.New(toshibaac.Config{
	Username: "someone@example.com", Password: oikade.Secret("toshiba"),
}))
```

## Setting it up

1. **Add your units in Toshiba's Home AC Control app**, if they aren't there already. The plugin sees what the app's account sees.
2. **Use the app's account:** `Username` is its username, usually your email address, and its password is a secret. Run `oikade secret set toshiba`, and paste it. It's read at each login, so a new password needs no deploy.

A refused password shows in `oikade status`, and the plugin waits for the secret to change, or an hour, rather than trying again, since too many failed logins could lock the account.

**The units' WiFi modules work only through Toshiba's cloud.** They listen for nothing on your network, so the plugin can't reach them locally. While the internet or Toshiba's cloud is down, every unit is offline, `oikade status` says why, a write fails at once, and a unit keeps its own remote and its own timers.

**A Safety claim on a unit can't act while the cloud is down.** The unit keeps its own controls, but Oikade can't turn it off until the cloud is back.

**Toshiba's API isn't published.** The plugin logs in as Toshiba's app does, as [KaSroka/Toshiba-AC-control](https://github.com/KaSroka/Toshiba-AC-control) worked it out, and Toshiba can change that. If it does, the plugin's status says the login failed. The plugin registers with the account as a phone would, once, under one name for the account, so it doesn't fill the account with devices.

## What it reports

Each unit's ID comes from its `DeviceUniqueId` in Toshiba's API, in lower case, so renaming it in the app keeps its rooms, its history and its Home app automations. Its name in the app is its suggested name.

| Capability | From the unit |
|---|---|
| `on` | its power |
| `mode` | `auto`, `heat`, `cool`, `dry` or `fan`, kept while it's off |
| `setpoint` | the temperature it aims for, in whole degrees Celsius. In 8 °C heating it's the real 8 °C, not the 24 the unit reports |
| `temperature` | its room sensor |

A mode the plugin doesn't know, such as one a later unit adds, isn't reported, and the host's log names it once. Fan speed, swing and Toshiba's other features aren't reported.

## Changing a unit

`on`, `mode` and `setpoint` can be written, from a rule or the Home app. A write returns once Toshiba's cloud has taken it, before the unit has changed: the unit's own report then shows what it did. Writing `on` true clears a unit's self-cleaning, which would keep it off, as the app does, and changing `mode` turns off a feature the new mode doesn't allow, such as 8 °C heating when it starts cooling.

A write the plugin knows the unit can't hold fails, rather than being rounded: a `setpoint` that isn't a whole degree, or a `mode` the unit doesn't do. Which setpoints a unit takes isn't known yet, so one it doesn't take is sent, and its report then shows the setpoint it kept. Which modes it does comes from what Toshiba's cloud says of it: a heater only heats, and an air conditioner without a heat pump does everything but heat.

### Units that share an outdoor unit

Indoor units on one outdoor unit, a multi-split, can't heat and cool at once. Toshiba's cloud takes such a write anyway, and the unit then reports the mode it was given without running it. The cloud doesn't say which units share an outdoor unit, so tell the plugin, by the IDs `oikade devices` shows, or the `DeviceUniqueId` as the app's API writes it:

```go
home.Plugin("toshiba", toshibaac.New(toshibaac.Config{
	Username: "someone@example.com", Password: oikade.Secret("toshiba"),
	OutdoorUnits: [][]string{{"a1b2c3d4e5f6", "0a0b0c0d0e0f"}},
}))
```

A write that would run one of them in a mode that clashes with another one running then fails and says which one: heating clashes with cooling and dry, and `auto` with heating, cooling and dry, since it may pick either. Fan runs beside any mode. Turning a unit off, changing its setpoint, and changing the mode of a unit that's off are never refused, so the Home app can set a mode before turning a unit on. A unit whose mode isn't known yet can't be turned on beside a running one, since it would run in whatever mode it last had. A refused write from the Home app shows on its tile until the unit's next report puts it back. An ID in `OutdoorUnits` that isn't on the account is logged at each login.

The check counts the writes sent to each unit, from before they're sent, until the unit reports, so two writes at once to units on one outdoor unit can't both pass. A write the unit never reports, say because it lost its WiFi, stops counting after a minute, and its last report is checked instead.

Toshiba's cloud passes on each change to a unit as it happens, and the unit's room temperature with its heartbeats. The plugin also reads every unit's state every 30 minutes, in case a change was missed, and reads the account's units again whenever it logs in: when it starts, when it reconnects, and each time its session's tokens run out. While the cloud doesn't answer, it tries again from 30 seconds up to 5 minutes apart. Told there were too many requests, it waits 10 minutes.
