# Devices

What Oikade knows about each device, how you name and type it, and how it reaches the Home app through the Matter bridge.

## The device list

The host owns the list of devices, in its registry in the data directory. A paired device enters it the moment a driver reports it, and it's usable at once, under a name copied from its app once. Oikade never writes to an app's names, or follows them after that.

```sh
oikade devices                               # every device, and how it's doing
oikade devices remove porch-light
oikade devices remove --plugin relays        # every device of a plugin you no longer import
oikade types                                 # write devices.go: a handle for every device
```

`oikade types` writes a Go file with a value for each device the host knows, with its name and a handle for each of its capabilities, such as `PorchLight.On`, and you commit it. So code naming a device that's gone, or a capability it no longer has, stops compiling, and builds need no host. A device your home [declares](#declaring-devices-in-code) is under its declared name, and the rest under their app's: `oikade types` builds your home (`--module`, `.` by default) and names the devices as its declarations will once you deploy, so a name you declare has its handle straight away. A home that doesn't build, as when it already names the handle of a device you've just declared, has its declarations read from its source instead, so write each one out with literal arguments, as below. `--out` and `--package` say where it goes. `oikade dev` and `hometest` simulate the devices the file has handles for, in the rooms and of the types your home declares, so a device you don't declare is in no room there. Two names that come out the same in Go, such as `a-1` and `a1`, fail until you rename one. You name, place and type a device by [declaring it in code](#declaring-devices-in-code), and a deploy applies that.

`remove` keeps the device on record with its endpoint, so hardware that comes back gets its name, room and endpoint back. Remove a device from its app first, because one that's still paired comes back when it next reports. A device stays until you remove it, reported or not. Removing a device ends its claims, and one that a Safety claim holds needs `--allow-safety`. Removing the last device a hold reads in a room warns, as in [Groups and rooms](automations.md#reading-a-room-or-a-group). `oikade devices` shows a device as `plugin not imported` when the running version doesn't import its plugin, and `oikade status` marks it the same way (`pluginNotImported` in `--json`). Both show a plugin's device your home doesn't declare as `not in your code`, and end with the line that would declare it as it is now, room and type included, to paste into your home if you want to name, place or type it (`declared` in `oikade devices --json`, and `declare` in `oikade status --json`).

If the host's data is lost, your next deploy gives its devices their names, rooms and types back as they report, since they're in your code. Matter endpoints are allocated again, and the Home app pairs afresh anyway, since its fabric went with the data. A backup (`oikade backup`, in [Running a home](running-a-home.md#upgrading-the-host)) restores everything.

## Declaring devices in code

Your home's code can say what a device is called, which room it's in and what it is, so a rename, a move or a swap of hardware is a change you review and deploy with the code that uses it ([OIK-295](https://linear.app/oikade/issue/OIK-295)):

```go
home.Device("porch-light").Hardware("zigbee", "0x00178801049a2b3c").Room("Porch").Is(oikade.OnOffLight)
home.Device("greenhouse-fan").Room("Greenhouse") // no hardware yet: the name is reserved
```

`Hardware` takes the name you import the device's plugin by, with `home.Plugin`, and the ID `oikade devices --json` shows. Declaring is optional: a device you don't declare works under its app's name, as above.

A deploy applies the declarations, matched by hardware, and lists what it changed: `Renamed lamp to porch-light`, `Moved porch-light to Porch`. A declared name keeps its Matter endpoint, so the Home app keeps its room and scenes. Change a declaration's ID to swap a failed device for new hardware: the name, room, claims and endpoint stay, so the Home app keeps its rooms and automations, and the new hardware's own entry goes. Stop declaring a device, and the next deploy gives it back its app's name, or removes a reserved name.

A declaration with no hardware, or hardware that hasn't reported, reserves its name until it does, and `oikade check` and the deploy say so. So you can write code for something that isn't paired yet, which does nothing until the hardware arrives. A reserved device has no capabilities yet, so `oikade types` gives it only its `Name`: until it reports, name a capability yourself, such as `oikade.Bool{Device: GreenhouseFan.Name, Capability: oikade.On}`, and run `oikade types` once it has. Give it its `Hardware` and deploy, and the name moves onto it.

`oikade check` refuses a name or a piece of hardware declared twice, a name a device can't have, a type that isn't one of the six on/off types, and a plugin your home doesn't import. A deploy refuses a type the device can't be, and a name an undeclared device has, and the old version keeps running. It also refuses a declared room or type that takes a device out of a Safety case's room write, such as a plug a Safety hold on the porch's plugs keeps off that you now declare a light, or move to the shed, and names the case, until you pass `--allow-safety-removal`. That's Safety cases: what a flow writes isn't known until it runs.

A rename takes the device's claims with it, matched by its hardware, so a Safety hold keeping `water-valve` closed holds it as `mains-valve` through the handover, with no `--allow-safety-removal`. Its cases are keyed by its name, so their claims, latches, pauses and averages move to the new name too, and the deploy doesn't count them as Safety rules gone. Hardware a declared name takes over keeps nothing of its own, so that's refused while something claims it, as is a reserved name that stops being declared. The host saves the declarations before applying them, so a host that stops in between applies them when it starts.

## What a device is

A device's capabilities say what it can do, and its device types say what it is, in Matter's names ([OIK-142](https://linear.app/oikade/issue/OIK-142)). `oikade devices` shows them in its `DEVICE TYPES` column, and `--json` as `deviceTypes`. Only a device that's `on` or off can be more than one thing, so that's the one you choose: `OnOffLight`, `DimmableLight`, `OnOffPlugInUnit`, `DimmablePlugInUnit`, `MountedOnOffControl` (a switch in the wall) or `MountedDimmableLoadControl` (a dimmer in the wall). A sensor's types follow from its capabilities, so a motion sensor with a thermometer is an `OccupancySensor` and a `TemperatureSensor`, a device with `locked` is a `DoorLock`, and one with `position` a `WindowCovering`. A device with a `setpoint` is a `Thermostat`, whatever its type, and one with a `mode` and an `on` as well is a `RoomAirConditioner`: its `on` and `temperature` are the heater's or the air conditioner's own, so it isn't also a plug or a `TemperatureSensor` ([OIK-20](https://linear.app/oikade/issue/OIK-20)).

The type your home declares with `Is` wins, then its driver's, if the driver knows: a Zigbee2MQTT light is an `OnOffLight`, or a `DimmableLight` with a brightness, and a plugin may report one. A Zigbee2MQTT switch doesn't say, because a relay can't tell what it switches. Otherwise a device with `brightness` is a `DimmableLight`, and one without an `OnOffPlugInUnit`, because a bare relay could be driving anything. Stop declaring its type, and it goes back to what its driver says. A dimmable type needs `brightness`, and an on/off type on a device that has it leaves dimming out of Matter. A reserved name takes any of the six, and once it's paired, a type its hardware can't be falls back as above.

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
| `setpoint` | °C, 0–50 | ✓ | Thermostat, heating only: the setpoint is OccupiedHeatingSetpoint, from 5 to 35 °C, `on` is SystemMode Off or Heat, and `temperature` is LocalTemperature. With a `mode`, a Room Air Conditioner instead |
| `mode` | `auto`, `heat`, `cool`, `dry` or `fan` | ✓ | With a `setpoint` and an `on`, a Room Air Conditioner: a Thermostat with heating, cooling and auto, and an On/Off with DeadFrontBehavior. SystemMode is `on` and `mode`: Off is `on` false, which keeps the mode, and Auto, Heat, Cool, FanOnly and Dry are `on` true in that mode, sent mode first so the unit doesn't start in its old one. Both setpoints are the one `setpoint`, from 5 to 35 °C, and in Heat or Cool a write to the other side's is taken and not sent. Without them, the bridge doesn't show it ([OIK-263](https://linear.app/oikade/issue/OIK-263)) |
| `position` | % closed, 0 open | ✓ | Window Covering, lift only: Matter's hundredths are `position` times 100. The Home app's slider writes it, and the tile shows where the blind is only once it reports. It shows the blind moving until it reports getting there, or for 2 minutes, and StopMotion writes the last reported position, failing if there's been none since the move ([OIK-23](https://linear.app/oikade/issue/OIK-23)). |
| `locked` | bool | ✓ | Door Lock, with no PINs or users. `true` while the bolt is fully thrown. LockDoor and UnlockDoor are timed commands, and LockState changes only when the lock reports, never on the command. It has no events yet, which the spec requires, so whether the Home app accepts it is unverified ([OIK-23](https://linear.app/oikade/issue/OIK-23)). |

## Matter bridge

Oikade reaches the Home app and other controllers through Matter ([OIK-76](https://linear.app/oikade/issue/OIK-76)). The bridge runs inside `oikade-host`, so nothing your code does reaches a controller: a deploy, a crash or a stuck process leaves sessions up and devices working ([OIK-143](https://linear.app/oikade/issue/OIK-143)). A host crash does take the bridge down, until it restarts.

```sh
oikade matter    # the pairing codes, whether it's ready to pair, and what it's paired with
```

The host needs host networking on its machine: the same L2 network as your controllers, with mDNS and IPv6. `--matter-port` moves the bridge from UDP 5540, and 0 turns Matter off. `--matter-interfaces` limits where it advertises. If its port is taken, or an interface isn't there, the host logs that and carries on without it, tries again every minute, and `oikade matter` says it's down and why: automations don't need it. While it's on no fabric, the commissioning window stays open for 15 minutes after the host starts. Restart the host to open it again.

Your home names the bridge, as a controller first shows it ([OIK-296](https://linear.app/oikade/issue/OIK-296)). It's `Oikade` until a deploy says otherwise:

```go
oikade.NewHome(oikade.Options{MatterName: "Cottage"}, /* … */) // at most 32 bytes
```

A deploy renames it in place, and the host keeps the name through a restart, so you don't pair again. The bridge's Basic Information says the new name at once, and the next commissioning window advertises it. Whether a controller that's already paired, such as the Home app, shows the new name isn't verified: it may keep the one it was given.

Every device on record with something Matter can show is on the bridge, unless your home [hides its plugin](#hiding-a-plugins-devices-from-matter), at its [registry](#devices) endpoint, as soon as it's paired, as its [device types](#what-a-device-is). Its label is its name, and a rename changes that in place: the endpoint and unique ID stay, so a controller keeps its room and scenes. A new device type rebuilds the device on the same endpoint, with the same unique ID. Removing a device takes it off. A device whose driver hasn't reported yet stays on, unreachable, so controllers don't see it vanish.

A controller's write goes through [arbitration](automations.md#arbitration) in the host, shown as `Matter`, and doesn't need your process. A capability a Safety claim holds refuses it, including a claim still on disk from before a restart: the write never reaches the device, the controller sees the value go back, and the log says who holds it. A device whose claim record can't be read counts as held at Safety: nothing writes it, nothing writes over the record, and the log says so. `oikade devices remove --allow-safety` clears it.

Add the bridge to a controller, such as the Home app, with the 11-digit manual code, or a QR code of the `MT:` payload. In the Home app, choose **Add Accessory**, then **More options**, then the bridge, and enter the code. Apple Home warns that it's an uncertified accessory: choose Add Anyway. It needs a home hub. To add the bridge to a second controller, open a window from the first one (in the Home app: the bridge's settings, Turn On Pairing Mode).

- **Writes:** a burst of commands from a controller, such as a swipe in the Home app, is gathered for 50 ms per device and capability, and only the last is written, unless the device is already in that state.
- **Unsupported devices:** a device none of whose capabilities map to Matter, such as one with only `link-quality`, keeps its endpoint ID but isn't on the bridge.

### Hiding a plugin's devices from Matter

Some platforms put their devices in the Home app themselves, such as a TaHoma hub its Somfy shutters. Oikade still drives them through its plugin, but on its bridge too they'd show in the Home app twice, in Siri and in every scene. Your home keeps them off Matter where it imports the plugin ([OIK-294](https://linear.app/oikade/issue/OIK-294)):

```go
home.Plugin("tahoma", overkiz.New(overkiz.Local{ /* … */ })).
	HideFromMatter().            // the TaHoma app puts these in the Home app itself
	ShowOnMatter("garage-door")  // except this one, which it doesn't
```

Your code still reads and sets them, and a tap on the platform's own tile reaches Oikade as the device changing by itself, as its own remote's would. A device the plugin reports later is hidden too. A deploy hides them once the version has settled, so one that fails leaves the bridge as it was. The host saves the setting before it applies it, so a restart keeps them off before your process connects.

`oikade check` says which devices a deploy would take off, and fails on a `ShowOnMatter` name that isn't one of that plugin's devices on record. `oikade deploy` says which it took off. A controller loses a device it took off, with its room and scenes there. A hidden device keeps its endpoint, so showing it again brings it back as the same device to Matter, though whether the Home app puts it back in its room isn't verified. `oikade devices` shows it as `hidden from Matter` (`"matter": false` in `--json`).
