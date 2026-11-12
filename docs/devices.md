# Devices

What Oikade knows about each device, how you name and type it, and how it reaches the Home app through the Matter bridge.

## The device list

The host owns the list of devices, in its registry in the data directory. A paired device enters it the moment a driver reports it, and it's usable at once, under a name copied from its app once. Oikade never writes to an app's names, or follows them after that.

```sh
oikade devices                               # every device, and how it's doing
oikade devices name lamp-2 porch-light       # call it something else
oikade devices room porch-light Porch        # move it between rooms
oikade devices type porch-light OnOffLight   # say what it is
oikade devices reserve shed-heater           # hold a name for hardware you haven't paired yet
oikade devices replace porch-light lamp-3    # move a name onto new hardware
oikade devices remove porch-light
oikade devices remove --plugin relays        # every device of a plugin you no longer import
oikade types                                 # write devices.go from the registry
```

`oikade types` writes a Go file with a value for each device, with its name and a handle for each of its capabilities, such as `PorchLight.On`, and you commit it. So code naming a device that's gone, or a capability it no longer has, stops compiling, builds need no host, and Git has a reviewable copy of your inventory. `--out` and `--package` say where it goes. The registry is the authority, and the file is its snapshot. Two names that come out the same in Go, such as `a-1` and `a1`, fail until you rename one. Running code loses track of a renamed device until you deploy, so run `oikade types` and deploy straight after. While a Safety rule runs, `name` and `replace` need `--allow-safety`, because a Safety rule that names the device stops protecting anything until that deploy.

`reserve` lets you write code for something that isn't paired yet, which does nothing until the hardware arrives. A reserved device has no capabilities yet, so `oikade types` gives it only its `Name`: until you pair it, name a capability with a handle of your own, such as `framework.Handle[bool]{Device: ShedHeater.Name, Capability: framework.On}`, and run `oikade types` once it's paired. Pair it, and `oikade devices replace shed-heater <its name>` moves the reserved name onto it. `replace` is also how a failed device is swapped: the name, room, claims and Matter endpoint stay, so the Home app keeps its rooms and automations. The new hardware's own entry goes, and the old hardware counts as new if it's ever paired again.

`remove` keeps the device on record with its endpoint, so hardware that comes back gets its name, room and endpoint back. Remove a device from its app first, because one that's still paired comes back when it next reports. A device stays until you remove it, reported or not. Renaming a device, moving its hardware onto another name, or restoring a file that would move its name, is refused while something claims it, because claims stay with a name. Removing a device ends its claims, and one that a Safety claim holds needs `--allow-safety`. `oikade devices` shows a device as `plugin not imported` when the running version doesn't import its plugin, and `oikade status` marks it the same way (`pluginNotImported` in `--json`).

If the registry is lost, `oikade devices restore devices.go` reads the file back: names, rooms, device types, reservations, arbitration settings and endpoint IDs, matched by hardware. The registry is the authority, so a file that disagrees with one in use is refused, which is what an old file does. After a real loss the drivers may have reported again first, under their apps' names, so pass `--force` there and the file wins. With `--force`, devices the file doesn't have stay, and get a new endpoint or name if the file gives theirs to something else. A claimed name never moves.

## What a device is

A device's capabilities say what it can do, and its device types say what it is, in Matter's names ([OIK-142](https://linear.app/oikade/issue/OIK-142)). `oikade devices` shows them in its `DEVICE TYPES` column, and `--json` as `deviceTypes`. Only a device that's `on` or off can be more than one thing, so that's the one you choose: `OnOffLight`, `DimmableLight`, `OnOffPlugInUnit`, `DimmablePlugInUnit`, `MountedOnOffControl` (a switch in the wall) or `MountedDimmableLoadControl` (a dimmer in the wall). A sensor's types follow from its capabilities, so a motion sensor with a thermometer is an `OccupancySensor` and a `TemperatureSensor`.

Yours wins, then its driver's, if the driver knows: a Zigbee2MQTT light is an `OnOffLight`, or a `DimmableLight` with a brightness, and a plugin may report one. A Zigbee2MQTT switch doesn't say, because a relay can't tell what it switches. Otherwise a device with `brightness` is a `DimmableLight`, and one without an `OnOffPlugInUnit`, because a bare relay could be driving anything. `oikade devices type DEVICE ""` goes back to what its driver says. A dimmable type needs `brightness`, and an on/off type on a device that has it leaves dimming out of Matter. Changing a device's type so that a Safety rule's room write limited to types would leave it out, such as a plug a Safety hold keeps on that you now call a light, needs `--allow-safety`, and the error names the rule. That's Safety holds and moments: what a flow writes isn't known until it runs. The type you set stays with the name, as a room does, through `replace`, a restart, `oikade types` and a restore. A reserved name takes any of the six, and once it's paired, a type its hardware can't be falls back as above.

## Capabilities

Drivers translate to and from a protocol-neutral set of capabilities, named as a person would say them ([OIK-181](https://linear.app/oikade/issue/OIK-181)). Matter, metrics and rules only ever see these, and any other name is unknown. A device with several sensors is one Matter endpoint with several device types, and a battery is a Power Source on it. [docs/capabilities.md](capabilities.md) lists each one's range and Matter attribute, generated from the registry.

| Capability | Type | Writable | Matter |
|---|---|---|---|
| `on` | bool | ✓ | Its [device type](#what-a-device-is): a light, a plug-in unit or a wall control |
| `brightness` | 0–100 | ✓ | Level Control, on a dimmable device type |
| `temperature` | °C | | Temperature Sensor |
| `humidity` | % | | Humidity Sensor |
| `open` | bool | | Contact Sensor (Boolean State, `true` for closed). `true` means open, the inverse of Zigbee2MQTT's `contact`. |
| `occupied` | bool | | Occupancy Sensor (passive infrared) |
| `battery` | % | | Power Source, with a warning below 20% |
| `link-quality` | 0–255 | | (metrics only) |

## Matter bridge

Oikade reaches the Home app and other controllers through Matter ([OIK-76](https://linear.app/oikade/issue/OIK-76)). The bridge runs inside `oikade-host`, so nothing your code does reaches a controller: a deploy, a crash or a stuck process leaves sessions up and devices working ([OIK-143](https://linear.app/oikade/issue/OIK-143)). A host crash does take the bridge down, until it restarts.

```sh
oikade matter    # the pairing codes, whether it's ready to pair, and what it's paired with
```

The host needs host networking on its machine: the same L2 network as your controllers, with mDNS and IPv6. `--matter-port` moves the bridge from UDP 5540, and 0 turns Matter off. `--matter-name` names it, and `--matter-interfaces` limits where it advertises. If its port is taken, or an interface isn't there, the host logs that and carries on without it, tries again every minute, and `oikade matter` says it's down and why: automations don't need it. While it's on no fabric, the commissioning window stays open for 15 minutes after the host starts. Restart the host to open it again.

Every device on record with something Matter can show is on the bridge, at its [registry](#devices) endpoint, as soon as it's paired, as its [device types](#what-a-device-is). Its label is its name, and a rename changes that in place: the endpoint and unique ID stay, so a controller keeps its room and scenes. A new device type rebuilds the device on the same endpoint, with the same unique ID. Removing a device takes it off. A device whose driver hasn't reported yet stays on, unreachable, so controllers don't see it vanish.

A controller's write goes through [arbitration](automations.md#arbitration) in the host, shown as `Matter`, and doesn't need your process. A capability a Safety claim holds refuses it, including a claim still on disk from before a restart: the write never reaches the device, the controller sees the value go back, and the log says who holds it. A device whose claim record can't be read counts as held at Safety: nothing writes it, nothing writes over the record, and the log says so. `oikade devices remove --allow-safety` clears it.

Add the bridge to a controller, such as the Home app, with the 11-digit manual code, or a QR code of the `MT:` payload. In the Home app, choose **Add Accessory**, then **More options**, then the bridge, and enter the code. Apple Home warns that it's an uncertified accessory: choose Add Anyway. It needs a home hub. To add the bridge to a second controller, open a window from the first one (in the Home app: the bridge's settings, Turn On Pairing Mode).

- **Writes:** a burst of commands from a controller, such as a swipe in the Home app, is gathered for 50 ms per device and capability, and only the last is written, unless the device is already in that state.
- **Unsupported devices:** a device none of whose capabilities map to Matter, such as one with only `link-quality`, keeps its endpoint ID but isn't on the bridge.
