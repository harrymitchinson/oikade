# eWeLink LAN plugin

This plugin connects relays with sensors that speak the eWeLink LAN protocol, such as SONOFF's TH Elite. It's a plugin your home imports (OIK-10). It needs no broker and no list of devices: it listens on the network, and any device it hears appears in `oikade list devices`, under its hardware's ref, `hardware/<plugin>/<ID>` by the name your home imports it by.

```go
home.Plugin("ewelink-lan", ewelinklan.New(ewelinklan.Config{
	AppID:     oikade.Var("EWELINK_APP_ID"), // your eWeLink developer app, for setup
	AppSecret: oikade.Var("EWELINK_APP_SECRET"),
	Humidity:  []string{"1000ddeeff"}, // these also announce humidity
}))
```

Each device's reports are encrypted with its own LAN key, which only your eWeLink account knows. Until the plugin has a device's key, the device is listed with what to do, and the Home app shows it as not responding. Setup fetches every device's key from your account, through an eWeLink developer app. Make the app on the eWeLink developer platform, under Platform → App Management, with the redirect URL `http://127.0.0.1:8765/callback`, where `oikade setup` listens. Put its ID and secret in your home's [`.env`](../../docs/running-a-home.md#your-values) as `EWELINK_APP_ID` and `EWELINK_APP_SECRET`, which the Config above passes in with `oikade.Var`, and deploy. Then run setup:

```sh
oikade status                               # 1000aabbcc  no LAN key: oikade setup ewelink-lan
oikade setup ewelink-lan                    # online within seconds, with no deploy
```

Then name each one in your home's code, as setup says:

```go
home.Device("greenhouse-heater").Hardware("ewelink-lan", "1000aabbcc")
```

Setup opens eWeLink's sign-in page in your browser, signs in through eWeLink's [OAuth2 API](https://coolkit-technologies.github.io/eWeLink-API/#/en/OAuth2.0), and lists your devices once. The host keeps each device's key (its `devicekey`), and setup changes only the keys that differ. It doesn't keep the token, so the host talks to eWeLink only while setup runs, and the plugin works on the LAN without the app. Setup also says what each device is called in the eWeLink app, with the [declaration](../../docs/devices.md) that gives it that name. Run it again for a device you pair later. A wrong app ID, secret or redirect URL shows when you sign in: fix it, deploy, and run setup again.

A device of type `th_plug`, such as a THR316D or THR320D, reports `on` and `temperature`, plus `humidity` if its ID is in `Humidity`, which is a list because a device's sensors decide what the Home app shows. A reading that isn't a number, as with humidity from a temperature-only probe, isn't reported, and the last value stays. A device listed in `Humidity` that sends none says so once in the host's log. Any other type isn't reported yet, and the log says so once. Each new type is checked against a real device before it's added. A device's firmware version is its reports' `fwVersion`, as the last report that had one said. The LAN protocol gives no manufacturer, model, serial number or hardware version (OIK-349).

For a few seconds after it powers up, a THR320D reports 25 °C, before it has read its probe. The eWeLink app shows it too, so the plugin passes it on. A rule that decides on the temperature should read an average, such as `PoolWater.Temperature.AverageOver(time.Hour)`, which a few seconds' spike barely moves.

A device that hasn't reported its relay for 30 seconds is offline, and stays in the list. Any eWeLink LAN device on your network appears, including ones you don't want: `oikade forget` hides one. It stays hidden through a restart if it's on the network when the plugin starts, and comes back if it was off then and is heard later.

The plugin follows up to 256 devices, and the log says once when it ignores one beyond that, so that a host on the network announcing made-up devices can't fill its memory.

## Security

eWeLink LAN traffic is encrypted with each device's key, but Oikade can't tell a replayed report from a new one. Keep these devices on a network you trust.
