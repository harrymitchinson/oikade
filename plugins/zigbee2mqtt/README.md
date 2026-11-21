# Zigbee2MQTT plugin

This plugin brings the Zigbee devices of a Zigbee2MQTT you run into Oikade. It's a plugin your home imports, and it speaks Zigbee2MQTT's MQTT API on the broker Zigbee2MQTT uses. Pairing, Zigbee2MQTT's configuration and its network key stay yours: the plugin changes none of them.

## What you need

- **A Zigbee2MQTT you run,** with its coordinator. Oikade doesn't start, stop or update it, and you pair devices in the Zigbee2MQTT web UI.
- **The MQTT broker it uses,** such as Mosquitto, which the host's machine can reach.
- **Two settings in Zigbee2MQTT's `configuration.yaml`,** which `oikade setup` checks:

```yaml
availability:
  enabled: true      # so a device that's gone reads offline
device_options:
  retain: true       # so a sensor's values survive a host restart
```

## Setting it up

Import the plugin in your home, under the name you'll use for it:

```go
home.Plugin("zigbee", zigbee2mqtt.New(zigbee2mqtt.Config{
	Broker:   "mqtt://oikade@broker.home:1883", // as the host reaches it
	Password: oikade.Var("BROKER_PASSWORD"),    // leave out the user and this for none
}))
```

| `Config` field | What it is |
| --- | --- |
| `Broker` | Required. The broker Zigbee2MQTT uses, as the host's machine reaches it, such as `mqtt://broker.home:1883`. It takes `mqtt://`, `tcp://`, `mqtts://`, `ssl://`, `tls://`, `ws://` and `wss://`. For a broker with a password, name the user in the URL, as in `mqtt://oikade@broker.home:1883`, but never the password. |
| `Password` | The broker's password, from your values: `oikade.Var("BROKER_PASSWORD")`, with `BROKER_PASSWORD` in your home's [`.env`](../../docs/running-a-home.md#your-values), so it isn't in your code. |
| `BaseTopic` | Zigbee2MQTT's `mqtt.base_topic`, if it isn't the default, `zigbee2mqtt`. |

A broker named in `.local`, such as `mqtt://broker.local:1883`, is found over mDNS for every scheme but WebSockets. Over `ws://` and `wss://` it's found only if the system's resolver finds it, such as from `/etc/hosts`.

A new password goes up with `oikade deploy`, which restarts the plugin. If the plugin can't reach the broker within a minute of starting, it stops, and the host starts it again with a backoff.

Then check it with `oikade setup`, naming the plugin as your home does. On the host, the plugin checks that it reaches the broker, that Zigbee2MQTT is there under the base topic and isn't offline, and the two settings above, and says what to set for any that's wrong:

```console
$ oikade setup zigbee
Starting the setup of zigbee on this machine's host
Broker mqtt://localhost:1883: connected
Zigbee2MQTT 2.6.0 found
availability is off in Zigbee2MQTT: a device that's gone won't read offline. Set availability.enabled: true
Error: zigbee isn't set up: zigbee needs 1 change in Zigbee2MQTT's configuration.yaml. Run oikade setup zigbee again after it
```

Once both are on, it ends with how many devices are paired, such as `zigbee is set up: 5 devices paired`. The plugin reads only Zigbee2MQTT's version and these settings from its `bridge/info`, which also holds the network key. Availability counts as on with `availability: true` too, and, in a 1.x configuration, with an `availability` object of timeouts or an `advanced.availability_timeout`.

Until both settings are on, or if Zigbee2MQTT isn't heard on the broker within 5 seconds of the plugin starting, `oikade status` says the plugin needs its setup.

## Your devices

Every device paired in Zigbee2MQTT is in Oikade at once, but the coordinator and devices Zigbee2MQTT doesn't support. Its ID is its IEEE address, such as `0x00124b0000000004`, so its ref is `hardware/zigbee/0x00124b0000000004`. Its name is its friendly name when it's first seen, made into a name Oikade takes, and renaming it in the Zigbee2MQTT web UI later changes neither. To name it in your code, give its ID:

```go
home.Device("desk-plug").Hardware("zigbee", "0x00124b0000000004")
```

Its manufacturer and model are its definition's `vendor` and `model`, and its firmware version its `software_build_id`, if the device gave Zigbee2MQTT one. Zigbee has no serial number or hardware version to report. A device that leaves Zigbee2MQTT stays on record, not found and offline.

## What it reports

A device's capabilities come from the exposes at the root of its `bridge/devices` entry:

| Zigbee2MQTT expose | Capability | Notes |
| --- | --- | --- |
| `light`'s `state` | `on` | Writable. A light with a `brightness` is a dimmable light, and one without an on/off light. |
| `light`'s `brightness` | `brightness` | Writable. Scaled from the expose's maximum, or 254 if it has none, to 0–100. |
| `switch`'s `state` | `on` | Writable. A relay says nothing about what it switches, so it's a plug until your home says what it is with `Is`. |
| `cover`'s `position` | `position` | Writable. Reversed both ways: Zigbee2MQTT's 100 is open, and Oikade's 100 closed, so its 30 is `position` 70. Leave Zigbee2MQTT's `invert_cover` off, or the position is reversed twice. |
| `lock`'s `state` and `lock_state` | `locked` | Writable, with `state`'s `LOCK` and `UNLOCK`. Read from `lock_state`, where the bolt is: only `locked` is locked, and any other value, such as `not_fully_locked`, isn't. A lock with no `lock_state` is read from `state`. |
| `contact` | `open` | Reversed: Zigbee2MQTT's `contact: true` is shut. |
| `occupancy` | `occupied` | |
| `temperature`, `humidity`, `battery` and `linkquality` | `temperature`, `humidity`, `battery` and `link-quality` | |

A device with both a light and a switch at its root is switched by the first. A value Oikade can't read, such as a battery that isn't a number, is dropped, and the rest of the message is taken.

### Availability

A device's availability is what Zigbee2MQTT publishes for it, and a device it says nothing about is online. While Zigbee2MQTT's `bridge/state` says it's offline, or the plugin has lost the broker, every device reads offline and writes fail. Your automations and Matter carry on, and the host's log says when Zigbee2MQTT changes state. The plugin reconnects by itself, and what Zigbee2MQTT retains brings the state back.

### Values after a restart

Values Zigbee2MQTT retains come back with the plugin. After each device list, the plugin asks Zigbee2MQTT with a `/get` for each value nothing has reported, one every 200 ms, where the device can be asked for it and Zigbee2MQTT doesn't say it's offline. A light or a plug answers, so its state is back straight after a restart even without `retain`. A battery sensor sleeps and can't be asked, so without `retain` its values stay unknown until it next reports. A reply is taken in as a retained value is, so it fires nothing a retained value wouldn't.

### Writes

A write goes to the device's IEEE address, `<base>/<IEEE address>/set`, so a rename can't send it elsewhere, and returns once the broker has it. Zigbee2MQTT doesn't confirm it: what the device reports next is what it did.

## What it can't do

- Pair, remove or configure devices, or change anything in Zigbee2MQTT: that's the Zigbee2MQTT web UI's.
- Serve an endpoint's features, such as a second gang's `state_l2`.
- Serve exposes the table doesn't list, such as a light's colour or colour temperature, or a plug's power.
- Serve Zigbee2MQTT's groups.
- Serve a cover with no `position`, which only opens and closes.
- Ask a sleeping sensor for its values.

## Fixtures from your devices

`testdata/fixtures` holds one file per device model: its entry in `bridge/devices`, and the state and command payloads heard for it. `TestGolden` replays each through the plugin and the Matter mapping, in `make test`, and compares the result with `testdata/golden`. To add your own models, record them from your broker:

```sh
go test ./plugins/zigbee2mqtt -run TestRecord -timeout 0 -args -record mqtt://localhost:1883 -record.for 10m
go test ./plugins/zigbee2mqtt -run TestGolden -args -update
```

The recorder only subscribes to `zigbee2mqtt/#`, or `-record.base` if yours differs: it never publishes, and changes nothing in Zigbee2MQTT. Use your devices while it listens, so it hears their states, and send them commands, such as with `oikade set` on a running host, so it hears those too.

It keeps each model's distinct payloads, adds to an earlier recording and replaces a hand-written fixture. It drops what describes one device rather than the model (addresses, bindings, `include_device_information`'s device, `last_seen`), and replaces every IEEE address and friendly name with `0x0000000000000001` and every IPv4 address with `192.0.2.1`. A model whose fixture would still name a device is left out, and named. Read the fixtures and the golden diff before committing them.
