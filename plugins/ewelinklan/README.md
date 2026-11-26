# eWeLink LAN plugin

This plugin serves relays with sensors that speak the eWeLink LAN protocol, such as SONOFF's TH Elite. It talks to them on your network, with no broker and no list of devices: any device it hears appears in `oikade list devices` as `hardware/<plugin>/<ID>`, where `<plugin>` is the name your home imports it by.

## Setting it up

Each device's reports are encrypted with its own LAN key, which only your eWeLink account knows. The plugin's setup fetches every device's key from your account once, through an eWeLink developer app, and the host keeps them. After that the plugin works on your network alone.

1. **Make a developer app** on the eWeLink developer platform, under Platform → App Management, with the redirect URL `http://127.0.0.1:8765/callback`, where `oikade setup` listens.
2. **Import the plugin,** and put the app's ID and secret in your home's `.env` ([Your values](../../docs/running-a-home.md#your-values)):

```go
home.Plugin("ewelink-lan", ewelinklan.New(ewelinklan.Config{
	AppID:     oikade.Var("EWELINK_APP_ID"),
	AppSecret: oikade.Var("EWELINK_APP_SECRET"),
	Humidity:  []string{"1000ddeeff"}, // these also report humidity
}))
```

```sh
# .env
EWELINK_APP_ID=…
EWELINK_APP_SECRET=…
```

3. **Deploy.** Until the plugin has a device's key, the device is listed with what to do, and the Home app shows it as not responding. Before the first setup, `oikade status` says `Plugin ewelink-lan needs setup: run oikade setup ewelink-lan`. After it, a device without a key, such as one you pair later, shows `no LAN key: oikade setup ewelink-lan` as its own problem.
4. **Run setup,** from any machine that reaches the host. It opens eWeLink's sign-in page in your browser, signs in through eWeLink's [OAuth2 API](https://coolkit-technologies.github.io/eWeLink-API/#/en/OAuth2.0), and lists your devices once. The host keeps each device's key (its `devicekey`), and the devices come online within seconds, with no deploy:

```console
$ oikade setup ewelink-lan
…
1000aabbcc is "Greenhouse heater" in the eWeLink app: oikade.Label("Greenhouse heater") on ewelink-lan/1000aabbcc's line in your home's code shows it so in the Home app
ewelink-lan is set up: 2 LAN keys kept, 0 already up to date
```

5. **Label each device in your home's code,** as setup says, since the LAN protocol doesn't say a device's name in the eWeLink app. `oikade types` adds its line, and an [`oikade.Label`](../../docs/devices.md#declaring-devices-in-code) on it is the name the Home app shows:

```go
var GreenhouseHeater = oikade.Device[oikade.OnOffPlugInUnit]("ewelink-lan/1000aabbcc", oikade.Label("Greenhouse heater"))
```

| `ewelinklan.Config` field | What it is |
|---|---|
| `AppID` | Your eWeLink developer app's ID. Only setup uses it. |
| `AppSecret` | That app's secret. Only setup uses it. |
| `Humidity` | The IDs of the devices that also report humidity. |

Setup keeps no token, so the host talks to eWeLink only while setup runs. Run it again for a device you pair later: it changes only the keys that differ. A wrong app ID, secret or redirect URL shows when you sign in: fix it, deploy, and run setup again. A device shared with you comes without its key, so sign in with the account that owns it.

## What it reports

A device's ID is its eWeLink device ID, such as `1000aabbcc`. Its firmware version is the `fwVersion` of its last report that had one. The LAN protocol gives no manufacturer, model, serial number or hardware version.

| Type | Devices | Capabilities |
|---|---|---|
| `th_plug` | SONOFF THR320D (TH Elite), and any other device of that type | `on`, `temperature`, and `humidity` if its ID is in `Humidity` |

`Humidity` is a list because a device's sensors decide what the Home app shows, so they're stated rather than worked out from its reports. A reading that isn't a number, as with humidity from a temperature-only probe, isn't reported, and the last value stays. A device listed in `Humidity` that sends no humidity says so once in the host's log.

A write to `on` returns once a newer report shows the relay has moved, or fails after 10 seconds. If a report from the last 30 seconds already shows the relay there, it returns once the device has taken the command.

For a few seconds after it powers up, a THR320D reports 25 °C, before it has read its probe. The eWeLink app shows it too, so the plugin passes it on. A rule that decides on the temperature should read an average, such as `GreenhouseHeater.Temperature.AverageOver(10 * time.Minute)`, which a few seconds' spike barely moves.

## Online and offline

The plugin asks the network for devices every 15 seconds. A device that hasn't reported its relay for 30 seconds is offline, and stays in the list. A device with no LAN key, or the wrong one, is offline too, and its state in `oikade status` says to run setup.

Any eWeLink LAN device on your network appears, including ones you don't want, such as a neighbour's: `oikade forget` hides one. It stays hidden through a restart if it's on the network when the plugin starts, and comes back if it was off then and is heard later.

## What it can't do

- **Any other type of device isn't reported yet,** nor one that doesn't encrypt its reports, and the host's log names its type once. Each new type is checked against a real device before it's added.
- **It follows up to 256 devices,** and the log says once when it ignores one beyond that, so that a host on the network announcing made-up devices can't fill its memory.

## Security

eWeLink LAN traffic is encrypted with each device's key, but Oikade can't tell a replayed report from a new one. Keep these devices on a network you trust.
