# Zigbee2MQTT plugin

This plugin connects Zigbee devices through Zigbee2MQTT. It's a plugin your home imports ([OIK-155](https://linear.app/oikade/issue/OIK-155)). You run Zigbee2MQTT and an MQTT broker such as Mosquitto yourself. Oikade connects to them, and doesn't start them or write their configuration.

```go
home.Plugin("zigbee", zigbee2mqtt.New(zigbee2mqtt.Config{
	Broker:   "mqtt://oikade@broker.home:1883", // as the host reaches it
	Password: oikade.Secret("broker"),       // leave out the user and this for none
}))
```

`Broker` is the broker Zigbee2MQTT uses, as the host's machine reaches it ([OIK-296](https://linear.app/oikade/issue/OIK-296)). A broker with a password names the user in the URL, and the password is a [secret](../../docs/running-a-home.md#secrets), never in the URL: `oikade secret set broker` sets it, and the plugin reads it each time it connects, so a new one needs no deploy. Until it's set, the plugin waits for it and says so in `oikade status`. A broker named in `.local`, such as `mqtt://broker.local:1883`, is found over mDNS over `mqtt://` or `mqtts://`, but over WebSockets only if the system's resolver finds it, such as from `/etc/hosts`. `BaseTopic` is for a Zigbee2MQTT whose `mqtt.base_topic` isn't the default, `zigbee2mqtt`.

Then check it with `oikade setup zigbee`, naming the plugin as your home does ([OIK-276](https://linear.app/oikade/issue/OIK-276)). On the host, the plugin checks that it reaches the broker, that Zigbee2MQTT is there under the base topic, and two of Zigbee2MQTT's settings, which it names with what to set if they're wrong:

```console
$ oikade setup zigbee
Starting the setup of zigbee on this machine's host
Broker mqtt://localhost:1883: connected
Zigbee2MQTT 2.6.0 found
availability is off in Zigbee2MQTT: a device that's gone won't read offline. Set availability.enabled: true
Error: zigbee isn't set up: zigbee needs 1 change in Zigbee2MQTT's configuration.yaml. Run oikade setup zigbee again after it
```

```yaml
availability:
  enabled: true      # so a device that's gone reads offline
device_options:
  retain: true       # so a sensor's values survive a host restart
```

It changes nothing in Zigbee2MQTT: its configuration is yours. It reads them from Zigbee2MQTT's `bridge/info`, and reads nothing else there, which holds the network key. Until both are set, or while Zigbee2MQTT isn't heard on the broker within 5 seconds of the plugin starting, `oikade status` says the plugin needs its setup. Setup also says when Zigbee2MQTT's `bridge/state` says it's offline. Availability is on in a 1.x configuration with `advanced.availability_timeout` too.

After each device list, the plugin asks Zigbee2MQTT with a `/get` for each value nothing has reported, one every 200ms, if the device can be asked for it ([OIK-53](https://linear.app/oikade/issue/OIK-53)). A light or a plug answers, so its state is back straight after a restart even without `retain`. A battery sensor sleeps and can't be asked, so without `retain` its values stay unknown until it next reports. A value that's retained is never asked for, and a reply is taken in just as a retained value is, so the first value after a host restart fires nothing.

Every paired device enters the registry by its IEEE address, except the coordinator and devices Zigbee2MQTT doesn't support, and is usable at once. Its friendly name is copied once, as its name. When Zigbee2MQTT says it's offline, or the broker can't be reached, its devices read offline and writes are refused, which the host's log shows. Your automations and Matter carry on.

## What devices become

Each device's capabilities come from the exposes in its `bridge/devices` entry, at its root. An endpoint's features, such as a second gang's `state_l2`, aren't served yet.

| Zigbee2MQTT expose | Capability | Notes |
| --- | --- | --- |
| `light`'s `state` | `on` | A dimmable light with a `brightness`, an on/off light without one. |
| `light`'s `brightness` | `brightness` | Scaled from the expose's maximum, 254 if it has none, to 0–100. |
| `switch`'s `state` | `on` | A relay says nothing about what it switches, so it's a plug unless you say otherwise. |
| `cover`'s `position` | `position` | Reversed both ways: Zigbee2MQTT's 100 is open, and Oikade's 100 closed, so its 30 is `position` 70. A cover with no `position` only opens and closes, and isn't served. Leave Zigbee2MQTT's `invert_cover` off, or the position is reversed twice. |
| `lock`'s `state` and `lock_state` | `locked` | Read from `lock_state`, where the bolt is: only `locked` is locked, and any other value, such as `not_fully_locked`, isn't. A `/get` names `state`, which reads both. A lock with no `lock_state` is read from `state`. Set with `state`'s `LOCK` or `UNLOCK`. |
| `contact` | `open` | Reversed: Zigbee2MQTT's `contact: true` is shut. |
| `occupancy` | `occupied` | |
| `temperature`, `humidity`, `battery` and `linkquality` | `temperature`, `humidity`, `battery` and `link-quality` | |

## Fixtures from your devices

`testdata/fixtures` holds one file per device model: its entry in `bridge/devices`, and the state and command payloads heard for it. `TestGolden` replays each through the plugin and the Matter mapping in `make test`, and compares the result with `testdata/golden` ([OIK-42](https://linear.app/oikade/issue/OIK-42)). To add your own models, record them from your broker:

```sh
go test ./plugins/zigbee2mqtt -run TestRecord -timeout 0 -args -record mqtt://localhost:1883 -record.for 10m
go test ./plugins/zigbee2mqtt -run TestGolden -args -update
```

The recorder only subscribes to `zigbee2mqtt/#` (`-record.base` if yours differs): it never publishes, and changes nothing in Zigbee2MQTT. Use your devices while it listens, so it hears their states, and send them commands from an MQTT client, such as `oikade set` on a running host, so it hears those too. It keeps each model's distinct payloads, adds to an earlier recording and replaces a hand-written fixture. It drops what describes one device rather than the model (addresses, bindings, `include_device_information`, `last_seen`), replaces every IEEE address and friendly name with `0x0000000000000001`, and leaves out, and names, any model whose fixture would still name a device. Read the fixtures and the golden diff before committing them.
