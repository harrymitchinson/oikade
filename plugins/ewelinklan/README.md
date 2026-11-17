# eWeLink LAN plugin

This plugin connects relays with sensors that speak the eWeLink LAN protocol, such as SONOFF's TH Elite. It's a plugin your home imports ([OIK-10](https://linear.app/oikade/issue/OIK-10)). It needs no broker and no list of devices: it listens on the network, and any device it hears appears in `oikade devices`, under its hardware's ref, `hardware/<plugin>/<ID>` by the name your home imports it by.

```go
home.Plugin("ewelink-lan", ewelinklan.New(ewelinklan.Config{
	Humidity: []string{"1000ddeeff"}, // these also announce humidity
}))
```

Each device's reports are encrypted with its own LAN key, which is one [secret](../../docs/running-a-home.md#secrets) named for its ID. Until it has one, the device is listed with what to do, and the Home app shows it as not responding. Setup fetches every device's key from your eWeLink account:

```sh
oikade devices                              # 1000aabbcc  no LAN key: oikade setup ewelink-lan
oikade setup ewelink-lan                    # online within seconds, with no deploy
```

Then name each one in your home's code, as setup says:

```go
home.Device("greenhouse-heater").Hardware("ewelink-lan", "1000aabbcc")
```

The first time, setup asks for an eWeLink developer app's ID and secret, and keeps them as the secrets `ewelink-app-id` and `ewelink-app-secret`. Make the app on the eWeLink developer platform, under Platform → App Management, with the redirect URL `http://127.0.0.1:8765/callback`, where `oikade setup` listens. Setup then opens eWeLink's sign-in page in your browser, signs in through eWeLink's [OAuth2 API](https://coolkit-technologies.github.io/eWeLink-API/#/en/OAuth2.0), and lists your devices once. It keeps each device's key (its `devicekey`), and changes only the keys that differ. It doesn't keep the token, so the host talks to eWeLink only while setup runs. It also says what each device is called in the eWeLink app, with the [declaration](../../docs/devices.md) that gives it that name. Run it again for a device you pair later.

A wrong app ID or secret shows when you sign in: change it with `oikade secret set ewelink-app-secret`, then run setup again. You can also set a device's key by hand, with `oikade secret set 1000aabbcc`.

A device of type `th_plug`, such as a THR316D or THR320D, reports `on` and `temperature`, plus `humidity` if its ID is in `Humidity`, which is a list because a device's sensors decide what the Home app shows. A reading that isn't a number, as with humidity from a temperature-only probe, isn't reported, and the last value stays. A device listed in `Humidity` that sends none says so once in the host's log. Any other type isn't reported yet, and the log says so once. Each new type is checked against a real device before it's added.

A device that hasn't reported its relay for 30 seconds is offline, and stays in the list. Any eWeLink LAN device on your network appears, including ones you don't want: `oikade devices remove` hides one. It stays hidden through a restart if it's on the network when the plugin starts, and comes back if it was off then and is heard later.

The plugin follows up to 256 devices, and the log says once when it ignores one beyond that, so that a host on the network announcing made-up devices can't fill its memory.

## Security

eWeLink LAN traffic is encrypted with each device's key, but Oikade can't tell a replayed report from a new one. Keep these devices on a network you trust.
