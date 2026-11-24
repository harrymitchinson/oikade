# Hue plugin

This plugin serves the lights and motion sensors on a Philips Hue bridge, over the bridge's own API on your network. Your home imports it once for each bridge.

Each device appears in `oikade list devices` as `hardware/<plugin>/<ID>`, where `<plugin>` is the name your home imports it by.

## Setting it up

```go
home.Plugin("hue", hue.New(hue.Config{})).
	HideFromMatter() // the Hue bridge puts these in the Home app itself
home.Device("hall-light").Hardware("hue", "0x7cc6b6fffecb1814").Room("hall").Is(oikade.OnOffLight)
```

**The plugin can't get a key from the bridge yet,** so it says `has no key for the Hue bridge yet` in `oikade status`. Its setup, which asks you to press the bridge's link button, is still being built (OIK-384).

**A Hue bridge puts its devices in the Home app itself,** so on Oikade's Matter bridge too they'd show twice. `HideFromMatter()` keeps them off it ([Hiding a plugin's devices from Matter](../../docs/devices.md#hiding-a-plugins-devices-from-matter)).

| `hue.Config` field | What it is |
|---|---|
| `Address` | The bridge's IP address or host name, with its port if it isn't 443, for a bridge mDNS can't reach. Without it, the plugin finds the bridge over mDNS by its ID. |

**The bridge's certificate is checked,** so nothing else on your network can pose as the bridge and take its key. Philips Hue signs each bridge's certificate for its bridge ID, and the plugin carries Philips Hue's root certificates and checks the ID against the bridge it has a key for. A certificate it refuses shows in `oikade status`, with what it's for.

## What it reports

A device's ID is its Zigbee MAC, as Zigbee2MQTT writes it, such as `0x7cc6b6fffecb1814`. It stays the same on a new bridge, and when a device moves from the Hue bridge to Zigbee2MQTT, so only the plugin's name in `Hardware` changes. Its manufacturer, model, firmware version, name and room are as the Hue app has them.

| On the bridge | Device type | Capabilities |
|---|---|---|
| A light that only turns on and off | `OnOffLight` | `on` |
| A light that dims, including colour lights | `DimmableLight` | `on`, `brightness` |
| A plug, such as the Hue smart plug | `OnOffPlugInUnit` | `on` |
| A motion sensor | | `occupied`, `temperature`, `battery` |

- **`brightness`** is in percent, from 0 to 100, as the bridge has it, such as `49.8`.
- **A motion sensor** turned off in the Hue app isn't `occupied`. A value a sensor hasn't got yet, such as one just added, is left out.
- **A device is offline while the bridge can't reach it over Zigbee,** as the Hue app shows it unreachable.

## Writes

A write turns a light on or off, or sets its brightness, and returns once the bridge has taken it, before the light has changed. The light's own report then shows it, within a second or so.

The bridge takes about ten light commands a second, so the plugin sends at most one every 100 ms, in the order they came. A write that's still waiting its turn after 4 seconds, as in a scene of many lights, goes when its turn comes. A newer write to the same light's `on` or `brightness` takes the place of one still waiting, so the light ends where you last set it.

A write fails at once while the bridge doesn't answer. A bridge that doesn't answer a write within 5 seconds is taken as gone: every device goes offline, and the plugin finds and reads the bridge again. A write the bridge refuses, such as to a light it can't reach, fails with what the bridge said, and one refused after it stopped waiting is logged as `Could not write to Hue light`.

## Keeping up, and when things go wrong

The plugin reads everything on the bridge once, then follows the bridge's event stream, so a change shows in `oikade status` within a second or so. When a device or room is added, removed or changed in the Hue app, it reads everything again.

While the bridge doesn't answer, every device is offline and `oikade status` says why. The plugin tries again after 5 s, doubling up to a minute. A key the bridge refuses, as one removed in the Hue app, is tried again every minute. A stream the bridge ends, after a minute or more, is opened again at once, with the devices online meanwhile.

## What it can't do

- **Colour, colour temperature and a motion sensor's light level aren't reported,** until Oikade has capabilities for them.
- **Dimmer switches and buttons aren't reported.** The host's log names each kind once, as does any device with more than one light.
- **MotionAware areas,** where lights sense movement, aren't reported.
