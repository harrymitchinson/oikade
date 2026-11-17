# Toshiba Home AC Control plugin

This plugin connects Toshiba air conditioners with a WiFi module, through Toshiba Home AC Control, the cloud behind Toshiba's app ([OIK-256](https://linear.app/oikade/issue/OIK-256)). It's a plugin your home imports, once for each account. Every indoor unit in the account appears in `oikade devices`, under its ID, and on the Matter bridge as a Room Air Conditioner.

```go
home.Plugin("toshiba", toshibaac.New(toshibaac.Config{
	Username: "someone@example.com", Password: oikade.Secret("toshiba"),
}))
```

**For now it only reads the units.** A write to one fails, and the unit keeps its state. Changing a unit's power, mode and setpoint comes next.

## Setting it up

1. **Add your units in Toshiba's Home AC Control app**, if they aren't there already. The plugin sees what the app's account sees.
2. **Use the app's account:** `Username` is its username, usually your email address, and its password is a secret. Run `oikade secret set toshiba`, and paste it. It's read at each login, so a new password needs no deploy.

A refused password shows in `oikade status`, and the plugin waits for the secret to change, or an hour, rather than trying again, since too many failed logins could lock the account.

**The units' WiFi modules work only through Toshiba's cloud.** They listen for nothing on your network, so the plugin can't reach them locally. While the internet or Toshiba's cloud is down, every unit is offline, `oikade status` says why, and a unit keeps its own remote and its own timers.

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

Toshiba's cloud passes on each change to a unit as it happens, and the unit's room temperature with its heartbeats. The plugin also reads every unit's state every 30 minutes, in case a change was missed, and reads the account's units again whenever it logs in: when it starts, when it reconnects, and each time its session's tokens run out. While the cloud doesn't answer, it tries again from 30 seconds up to 5 minutes apart. Told there were too many requests, it waits 10 minutes.
