# eWeLink LAN plugin

This plugin connects relays with sensors that speak the eWeLink LAN protocol, such as SONOFF's TH Elite. It's a plugin your home imports ([OIK-10](https://linear.app/oikade/issue/OIK-10)). It needs no broker and no list of devices: it listens on the network, and any device it hears appears in `oikade devices`, under its ID.

```go
framework.Run(framework.NewHome(framework.Options{},
	framework.NewPlugin("ewelink-lan", ewelinklan.New(ewelinklan.Config{
		Humidity: []string{"1000ddeeff"}, // these also announce humidity
	})),
	// ...
))
```

Each device's reports are encrypted with its own LAN key, which is one [secret](../../README.md#secrets) named for its ID. Until it has one, the device is listed with what to do, and the Home app shows it as not responding:

```sh
oikade devices                              # 1000aabbcc  no LAN key: oikade secret set 1000aabbcc
oikade secret set 1000aabbcc                # online within seconds, with no deploy
oikade devices name 1000aabbcc greenhouse-heater
```

The LAN key is the device's `devicekey`, and only eWeLink's cloud API gives it out: [Getting a LAN key](#getting-a-lan-key) says how. Oikade itself never talks to the cloud.

A device of type `th_plug`, such as a THR316D or THR320D, reports `on` and `temperature`, plus `humidity` if its ID is in `Humidity`, which is a list because a device's sensors decide what the Home app shows. A reading that isn't a number, as with humidity from a temperature-only probe, isn't reported, and the last value stays. A device listed in `Humidity` that sends none says so once in the host's log. Any other type isn't reported yet, and the log says so once. Each new type is checked against a real device before it's added.

A device that hasn't reported its relay for 30 seconds is offline, and stays in the list. Any eWeLink LAN device on your network appears, including ones you don't want: `oikade devices remove` hides one. It stays hidden through a restart if it's on the network when the plugin starts, and comes back if it was off then and is heard later.

## Getting a LAN key

eWeLink calls the `devicekey` the device's factory key, so you need it once per device. Getting it uses eWeLink's [OAuth2 API](https://coolkit-technologies.github.io/eWeLink-API/#/en/OAuth2.0), and needs a small script, because a sign-in code lasts only 30 seconds. [OIK-276](https://linear.app/oikade/issue/OIK-276) is the idea of a command that does it for you.

1. **Make an app** on the eWeLink developer platform, under Platform → App Management. Set its redirect URL to one your script can listen on, such as `http://127.0.0.1:8765/callback`. Note the app's ID and secret.
2. **Open the sign-in page** in a browser:

   ```
   https://c2ccdn.coolkit.cc/oauth/index.html?clientId=APP_ID&seq=SEQ&authorization=SIGNATURE&redirectUrl=REDIRECT_URL&grantType=authorization_code&state=STATE&nonce=NONCE&showQRCode=false
   ```

   `SEQ` is the time in milliseconds, `NONCE` is 8 letters or digits, and `STATE` is anything. `SIGNATURE` is the Base64 HMAC-SHA256 of `APP_ID_SEQ` (the two joined by an underscore), keyed with the app secret. URL-encode each value.
3. **Sign in** with your eWeLink account. The browser goes to your redirect URL with `code` and `region` (`eu`, `us`, `as` or `cn`) in its query.
4. **Within 30 seconds, swap the code for a token** with `POST https://REGION-apia.coolkit.cc/v2/user/oauth/token` (`cn-apia.coolkit.cn` for `cn`). The body is `{"code":"CODE","redirectUrl":"REDIRECT_URL","grantType":"authorization_code"}`. Send it with the headers `Content-Type: application/json`, `X-CK-Appid: APP_ID`, `X-CK-Nonce: NONCE` and `Authorization: Sign SIGNATURE`. Here, `SIGNATURE` is the Base64 HMAC-SHA256 of the exact body you send, keyed with the app secret. The token is the answer's `data.accessToken`.
5. **List your devices** with `GET https://REGION-apia.coolkit.cc/v2/device/thing?num=0` and the header `Authorization: Bearer TOKEN`. Each entry of `data.thingList` has `itemData.deviceid` and `itemData.devicekey`.
6. **Give each key to Oikade:** run `oikade secret set DEVICEID`, and paste its `devicekey`.

The `devicekey` is all a device needs, so keep it like a password, and don't keep the token once you're done.

## Security

eWeLink LAN traffic is encrypted with each device's key, but Oikade can't tell a replayed report from a new one. Keep these devices on a network you trust.
