# Zigbee2MQTT plugin

This plugin connects Zigbee devices through Zigbee2MQTT. It's a plugin your home imports ([OIK-155](https://linear.app/oikade/issue/OIK-155)). You run Zigbee2MQTT and an MQTT broker such as Mosquitto yourself. Oikade connects to them, and doesn't start them or write their configuration.

```go
framework.Run(framework.NewHome(framework.Options{},
	framework.NewPlugin("zigbee", zigbee2mqtt.New(zigbee2mqtt.Config{})),
	// ...
))
```

```sh
oikade-host --home ./home --broker mqtt://localhost:1883
```

A password in the `--broker` URL reaches every plugin you import. `zigbee2mqtt.Config` takes `BaseTopic` if yours isn't Zigbee2MQTT's default, `zigbee2mqtt`.

Set these in Zigbee2MQTT's `configuration.yaml`:

```yaml
availability:
  enabled: true      # so a device that's gone reads offline
device_options:
  retain: true       # so a sensor's values survive a host restart
```

After each device list, the plugin asks Zigbee2MQTT with a `/get` for each value nothing has reported, one every 200ms, if the device can be asked for it ([OIK-53](https://linear.app/oikade/issue/OIK-53)). A light or a plug answers, so its state is back straight after a restart even without `retain`. A battery sensor sleeps and can't be asked, so without `retain` its values stay unknown until it next reports. A value that's retained is never asked for, and a reply is taken in just as a retained value is, so the first value after a host restart fires nothing.

Every paired device enters the registry by its IEEE address, except the coordinator and devices Zigbee2MQTT doesn't support, and is usable at once. Its friendly name is copied once, as its name. When Zigbee2MQTT says it's offline, or the broker can't be reached, its devices read offline and writes are refused, which the host's log shows. Your automations and Matter carry on.

## Fixtures from your devices

`testdata/fixtures` holds one file per device model: its entry in `bridge/devices`, and the state and command payloads heard for it. `TestGolden` replays each through the plugin and the Matter mapping in `make test`, and compares the result with `testdata/golden` ([OIK-42](https://linear.app/oikade/issue/OIK-42)). To add your own models, record them from your broker:

```sh
go test ./plugins/zigbee2mqtt -run TestRecord -timeout 0 -args -record mqtt://localhost:1883 -record.for 10m
go test ./plugins/zigbee2mqtt -run TestGolden -args -update
```

The recorder only subscribes to `zigbee2mqtt/#` (`-record.base` if yours differs): it never publishes, and changes nothing in Zigbee2MQTT. Use your devices while it listens, so it hears their states, and send them commands from an MQTT client, such as `oikade set` on a running host, so it hears those too. It keeps each model's distinct payloads, adds to an earlier recording and replaces a hand-written fixture. It drops what describes one device rather than the model (addresses, bindings, `include_device_information`, `last_seen`), replaces every IEEE address and friendly name with `0x0000000000000001`, and leaves out, and names, any model whose fixture would still name a device. Read the fixtures and the golden diff before committing them.
