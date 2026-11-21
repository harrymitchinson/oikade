// Package zigbee2mqtt is the plugin for a Zigbee2MQTT and an MQTT broker you run. A home imports
// it, and its Config says where the broker is:
//
//	home.Plugin("zigbee", zigbee2mqtt.New(zigbee2mqtt.Config{
//		Broker: "mqtt://oikade@broker:1883", Password: oikade.Var("BROKER_PASSWORD"),
//	}))
//
// It speaks Zigbee2MQTT's MQTT API under the base topic. It starts nothing, writes none of
// Zigbee2MQTT's configuration and holds none of its keys. Instance follows Zigbee2MQTT on the
// broker, and bridge reports what it hears through the SDK.
//
// Devices come from <base>/bridge/devices. Every device in it but the coordinator and those
// Zigbee2MQTT has no definition for is reported found, and discovered, under its IEEE address, such
// as 0x00124b0000000004, which a rename in the Zigbee2MQTT web UI doesn't change. Its capabilities
// come from the exposes at its root (bindings). A message for a name no list has resolved yet is
// held for the next list, since a broker replays retained topics in any order. After each list, a
// paced /get asks for each value nothing has reported, where the expose's access says it can be
// read and Zigbee2MQTT doesn't say the device is offline. Values go out with ReportState, because a
// sleeping sensor can't be asked.
//
// A write is a /set to the device's IEEE address, and Set returns once the broker has it. Nothing
// else confirms it: the result is the state Zigbee2MQTT publishes next.
//
// Zigbee2MQTT is down while its <base>/bridge/state, which is its Last Will, says offline, or while
// the plugin has lost the broker. Either marks every device offline and refuses writes.
//
// The README beside this file is the plugin's user docs.
package zigbee2mqtt
