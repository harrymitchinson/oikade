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
