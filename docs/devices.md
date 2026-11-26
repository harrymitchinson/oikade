# Devices

What Oikade knows about each device, how you name, place and type it, and how it reaches the Home app through the Matter bridge.

## How a device arrives

The host keeps the list of devices, which it calls the record. A device you pair in its platform's app is on record the moment its plugin reports it, and usable at once. Its name and room are copied from the app once, each made into a [name](#names), so the app's `Living Room` is `living-room`. Oikade never writes to an app's names, or follows them after that.

To name, place or type a device yourself, [declare it in your code](#declaring-devices-in-code), and a deploy applies that.

## Listing devices

```sh
oikade list devices          # every device on record, and how it's set up
oikade list devices --wide   # with its hardware and Matter endpoint, however narrow the terminal
oikade list devices --all    # with the forgotten devices too
```

```console
$ oikade list devices
NAME         ROOM   STATE                     DEVICE TYPES        CAPABILITIES                     PLUGIN
hall-lamp    hall   paired                    OnOffLight          on                               ewelink-lan
office-ac           paired, not in your code  RoomAirConditioner  mode, on, setpoint, temperature  toshiba
porch-light         plugin not imported       OnOffPlugInUnit     on                               ewelink-lan
study-blind  study  hidden from Matter        WindowCovering      position                         overkiz
Hint: --wide shows HARDWARE and ENDPOINT
To name, place or type a device that's not in your code, declare it in your home:
  home.Device("office-ac").Hardware("toshiba", "5f1c0c2e-77b3-4f8e-9d4a-1b2c3d4e5f60")
Hint: oikade status shows what each is doing now, and what's wrong with it
```

Its STATE is the device's record, never what's wrong with it now, which is [`oikade status`](working-with-a-home.md#status)'s:

| STATE | |
|---|---|
| `paired` | On record with its hardware |
| `reserved` | Your home declares the name, and it has no hardware yet. See [Reserving a name](#reserving-a-name) |
| `forgotten` | You [forgot](#forgetting-a-device) it. Only `--all` lists it |
| `plugin not imported` | The running version doesn't import its plugin. See [Removing a plugin](running-a-home.md#removing-a-plugin) |
| `hidden from Matter` | Your home [keeps its plugin's devices off Matter](#hiding-a-plugins-devices-from-matter) |
| `not in your code` | Your home doesn't declare it. The lines after the table say how to |

The table keeps to the terminal's width, and is whole when piped. The hardware, endpoint and device types columns go first, then the capabilities shorten, and `--wide` shows them all.

`--json` is an array of devices, each with its `ref`:

- **What the plugin says it is:** its `manufacturer`, `model`, `serialNumber`, `firmwareVersion` and `hardwareVersion`. Each is kept while the plugin says nothing of it, so a device that's offline still has them, and the Home app shows them on the bridged device too.
- **Where it came from:** its app's name and room as `appName` and `appRoom`, and its hardware's ID as `id` and ref as `hardware`.
- **Whether your home declares it,** as `declared`, and its room, as `roomDeclared`. `oikade status --json` has the line that would declare it as `declare`.

## Devices in your code

`oikade types` writes `devices.go`, a Go file with a value for each device the host knows: its name, and a handle for each of its capabilities, such as `PorchLight.On`. Commit it. Code naming a device that's gone, or a capability it no longer has, stops compiling, and builds need no host.

```sh
oikade types                  # write devices.go from the host's devices
oikade types --out - | less   # or read it first
```

- **Names come from your declarations.** `oikade types` builds your home (`--module`, `.` by default) and names the devices as its [declarations](#declaring-devices-in-code) will once you deploy, so a name you declare has its handle straight away. The rest are under their app's names.
- **A home that doesn't build,** as when it already uses the handle of a device you've just declared, has its declarations read from its source instead. So write each one out with literal arguments, as below.
- **Two names that come out the same in Go,** such as `a-1` and `a1`, fail until you rename one.
- **`--out FILE` and `--package NAME`** say where it goes, `devices.go` in package `main` by default.

`oikade dev` and `oikadetest` simulate the devices `devices.go` has handles for, in the rooms and of the types your home declares: see [Testing a home](testing.md).

## Forgetting a device

```sh
oikade forget porch-light        # several at once too, checked before any is forgotten
oikade forget --plugin relays    # every device of a plugin you no longer import
```

Forgetting a device ends its claims and takes it off the Matter bridge, and `forget` says both. One that a Safety claim holds needs `--allow-safety`. A device stays on record until you forget it, reported or not.

- **It keeps its endpoint,** so hardware that comes back gets its name, room and endpoint back.
- **A plugin's device stays forgotten** while the plugin lists it, and comes back if it leaves the list and returns, as a device re-paired in its app does. `forget` says which.
- **A simulated device** comes back when it next reports.
- **A device your home declares is refused:** delete its declaration and deploy, then forget it under its app's name.
- **Forgetting the last device a case reads in a room warns,** as in [Reading a room or a group](automations.md#reading-a-room-or-a-group).

## Online and offline

A device its plugin says is offline shows as offline in `oikade status`, with when it went and the plugin's reason, if it gives one. The values `oikade status` shows for it are the last it reported. `--json` has `"online": false` and `offlineSince`. The host logs `Device went offline` and `Device came back online` with the device, so its log says when.

## Names

Every name Oikade owns follows one rule: lowercase letters, digits and single dashes, starting with a letter, and at most 63 of them, such as `porch-light`. That's devices, rooms, groups, a decision's cases, flows, plugins and the API's clients, so a name needs no quoting anywhere you paste it. [Your values](running-a-home.md#your-values) have names of their own, as environment variables do, such as `TOSHIBA_PASSWORD`.

`oikade check` refuses a name in your code that doesn't follow the rule, and `oikade-host add` and `--outside-plugins` refuse one on the command line. Each suggests a name that would do: `"Living Room" isn't a room name: use lowercase letters, digits and single dashes, starting with a letter, such as "living-room"`.

Nothing rewrites a name you wrote. A name copied from an app is made into one instead: a letter with an accent becomes its letter, and each run of anything that's still not `a` to `z` or `0` to `9` a dash, so the app's `Entrée détecteur` is `entree-detecteur`. `oikade list devices --json` keeps the app's own as `appName` and `appRoom`.

## Refs

A ref names one thing, by its kind and then its name, and any parts, each a URI path segment. It's the same wherever Oikade prints or takes it, so you can paste it from one command into another:

| Kind | Ref |
|---|---|
| A device, and one of its capabilities | `device/desk-plug`, `device/desk-plug/on` |
| A room's or a group's capability | `room/hall/temperature`, `group/radiators/setpoint` |
| A decision's case | `case/desk-plug/on/night` |
| A flow | `flow/morning` |
| A room, a group | `room/living-room`, `group/downstairs` |
| A plugin, a client | `plugin/zigbee2mqtt`, `client/laptop` |
| A device's hardware, by its plugin and the plugin's ID for it | `hardware/zigbee2mqtt/0x00158d0001a2b3c4` |
| An outside change's claim: a Matter controller's, the device's own, `oikade set`'s | `matter`, `device`, `command` |

- **Commands take refs.** A command that takes one kind of thing takes its ref or its bare name: `oikade status desk-plug` or `oikade status device/desk-plug`.
- **Output prints them.** Each thing `--json` lists has its `ref`, and a claim's `source` in `oikade status --json` is one, so `oikade pause` takes a case's or a flow's straight from it. Answers and errors name a capability, a case or a flow by its ref, such as `device/desk-plug/on is false, not true: case/desk-plug/on/hall-too-humid holds it`.
- **Your code prints them.** A handle's `String` is its ref: `PorchLight.On` prints `device/porch-light/on`, and a room's or a group's capability `room/hall/temperature` or `group/radiators/setpoint`.
- **Logs and metrics use them.** Log lines give a rule, a claim's source or an average as a ref (`rule=case/desk-plug/on/night`, `source=matter`, `average=device/bathroom-sensor/humidity over=10m`), and metrics' `rule` label is a rule's ref.

Only a hardware ID is the plugin's and not Oikade's, so it's percent-encoded where it needs to be. `oikade list devices --wide` shows each plugin's device by its hardware's ref, such as `hardware/overkiz/io.11111111` for the Overkiz ID `io.11111111`, and `--json` has it as `hardware`. `Hardware` in your code takes the plugin's name and the raw ID, as `id` in `--json` and the line `oikade list devices` gives an undeclared device have it.

## Declaring devices in code

Your home's code can say what a device is called, which room it's in and what it is. A rename, a move or a swap of hardware is then a change you review and deploy with the code that uses it:

```go
home.Device("porch-light").Hardware("zigbee", "0x00178801049a2b3c").Room("porch").Is(oikade.OnOffLight)
home.Device("greenhouse-fan").Room("greenhouse") // no hardware yet: the name is reserved
```

`Hardware` takes the name your home imports the device's plugin by, with `home.Plugin`, and the ID `oikade list devices --json` shows. Without `Room`, a declared device stays in its app's room, and `Room("")` puts it in none. `Is` takes one of the [six on/off types](#what-a-device-is). Declaring is optional: a device you don't declare works under its app's name.

### What a deploy does with them

A deploy applies the declarations, matched by hardware, and lists what it changed: `Renamed lamp to porch-light`, `Moved porch-light to porch`. The host keeps the declarations before applying them, so a host that stops in between applies them when it starts.

- **A declared name keeps its Matter endpoint,** so the Home app keeps its room and scenes.
- **A rename moves nothing.** The host keeps a device's claims, latches, pauses and averages by its hardware, not its name, so a Safety hold keeping `water-valve` closed holds it as `mains-valve`, with no `--allow-safety`, and the handles `oikade types` writes don't change.
- **Stop declaring a device,** and the next deploy gives it back its app's name, or removes a reserved name.

### Swapping hardware

Change a declaration's ID to swap a failed device for new hardware, and run `oikade types`. The name, room and endpoint stay, so the Home app keeps its rooms and automations, and the new hardware's own entry goes. Claims don't: the host keeps them by hardware, so the new hardware's cases are new ones, and a Safety case on the old hardware is one gone, which the deploy refuses without `--allow-safety`. Hardware a declared name takes over keeps its own claims, as a rename does.

### Reserving a name

A declaration with no hardware, or hardware that hasn't reported, reserves its name until it does, and `oikade check` and the deploy say so. So you can write code for something that isn't paired yet, which does nothing until the hardware arrives.

A reserved device has no capabilities yet, so `oikade types` gives it only its `Name`. Until it reports, name a capability yourself, such as `oikade.Bool{Device: GreenhouseFan.Name, Capability: oikade.On}`, and run `oikade types` once it has. Give it its `Hardware` and deploy, and the name moves onto it, then run `oikade types` again: its handles have the hardware from then on, and what the reserved name held, such as a claim, isn't the hardware's. A reserved name that stops being declared is refused while something claims it.

### What's refused

`oikade check` refuses a name or a piece of hardware declared twice, a name a device can't have, a type that isn't one of the six on/off types, and a plugin your home doesn't import.

A deploy refuses a type the device can't be, and a name an undeclared device has, and the old version keeps running. It also refuses a declared room or type that takes a device out of a Safety case's room write, until you pass `--allow-safety`, and names the case. That's a plug a Safety hold on the porch's plugs keeps off that you now declare a light, or move to the shed. Only Safety cases count, because what a flow writes isn't known until it runs.

## What a device is

A device's capabilities say what it can do, and its device types say what it is, in Matter's names. `oikade list devices` shows them in its `DEVICE TYPES` column, and `--json` as `deviceTypes`.

Only a device that's on or off can be more than one thing, so that's the one you choose with `Is`:

| Type | |
|---|---|
| `OnOffLight`, `DimmableLight` | A light |
| `OnOffPlugInUnit`, `DimmablePlugInUnit` | A plug |
| `MountedOnOffControl` | A switch in the wall |
| `MountedDimmableLoadControl` | A dimmer in the wall |

Which one a device is comes from the first of these that says:

1. **Your home's `Is`.** Stop declaring its type, and it goes back to what its plugin says.
2. **Its plugin, if the plugin knows.** A Zigbee2MQTT light is an `OnOffLight`, or a `DimmableLight` with a brightness, and any plugin may report one. A Zigbee2MQTT switch doesn't say, because a relay can't tell what it switches.
3. **Its capabilities.** A device with `brightness` is a `DimmableLight`, and one without an `OnOffPlugInUnit`, because a bare relay could be driving anything.

A dimmable type needs `brightness`, and an on/off type on a device that has it leaves dimming out of Matter. A reserved name takes any of the six, and once it's paired, a type its hardware can't be falls back as above.

Every other type follows from the capabilities:

- **A sensor** is a type for each thing it senses, so a motion sensor with a thermometer is an `OccupancySensor` and a `TemperatureSensor`.
- **A device with `locked`** is a `DoorLock`, and one with `position` a `WindowCovering`.
- **A device with a `setpoint`** is a `Thermostat`, whatever its type, and one with a `mode` and an `on` as well is a `RoomAirConditioner`. Its `on` and `temperature` are the heater's or the air conditioner's own, so it isn't also a plug or a `TemperatureSensor`.

## Capabilities

Plugins translate to and from a set of capabilities named as a person would say them, and Matter, metrics and rules only ever see these. A device with several sensors is one Matter endpoint with several device types, and a battery is a Power Source on it. [Capabilities](capabilities.md) lists each one's type, range and Matter attribute.

| Capability | Means | On the Matter bridge |
|---|---|---|
| `on` | On or off | Its [device type](#what-a-device-is): a light, a plug-in unit or a wall control |
| `brightness` | A light's brightness, 0–100% | Level Control, on a dimmable device type |
| `temperature` | °C | Temperature Sensor |
| `humidity` | Relative humidity, % | Humidity Sensor |
| `open` | True while a door or window is open | Contact Sensor, whose Boolean State is the other way round: true while it's shut |
| `occupied` | True while motion or presence is detected | Occupancy Sensor (passive infrared) |
| `battery` | Charge, % | Power Source, with a warning below 20% |
| `link-quality` | A radio link's quality, 0–255 | Not shown: metrics only |
| `setpoint` | The temperature a heater aims for, °C | Thermostat, heating only. The setpoint is OccupiedHeatingSetpoint, from 5 to 35 °C, `on` is SystemMode Off or Heat, and `temperature` is LocalTemperature. With a `mode`, a Room Air Conditioner instead |
| `mode` | What an air conditioner does while it's on: `auto`, `heat`, `cool`, `dry` or `fan` | With a `setpoint` and an `on`, a Room Air Conditioner: a Thermostat with heating, cooling and auto, and an On/Off with DeadFrontBehavior. Without them, not shown |
| `position` | How far closed a blind is: 0% open, 100% closed | Window Covering, lift only |
| `locked` | True while a lock's bolt is fully thrown | Door Lock, with no PINs or users |

Some of them behave in ways worth knowing:

- **An air conditioner's SystemMode** is `on` and `mode` together. Off is `on` false, which keeps the mode. Auto, Heat, Cool, FanOnly and Dry are `on` true in that mode, sent mode first so the unit doesn't start in its old one. Both setpoints are the one `setpoint`, from 5 to 35 °C, and in Heat or Cool a write to the other side's is taken and not sent.
- **A blind's position** is Matter's hundredths divided by 100. The Home app's slider writes it, and the tile shows where the blind is only once it reports. It shows the blind moving until it reports getting there, or for 2 minutes. StopMotion writes the last reported position, and fails if there's been none since the move.
- **A lock's** LockDoor and UnlockDoor are timed commands, and LockState changes only when the lock reports, never on the command. It has no events yet, which the spec requires, so whether the Home app accepts it is unverified.

## The Matter bridge

Oikade reaches the Home app and other controllers through a Matter bridge inside `oikade-host`. Nothing your code does reaches a controller: a deploy, a crash or a stuck process leaves sessions up and devices working. A host crash does take the bridge down, until it restarts.

```sh
oikade status matter   # the pairing codes, whether it's ready to pair, and what it's paired with
oikade pair matter     # open the window to pair, for 15 minutes, or --for 3m to 15m
```

### Pairing

While the bridge is on no fabric, the window to pair stays open for 15 minutes after the host starts, and `oikade status matter` draws the QR code in the terminal for the Home app or a phone's camera to scan. Add the bridge with the 11-digit manual code, or the QR code's `MT:` payload.

In the Home app, choose **Add Accessory**, then **More options**, then the bridge, and enter the code. The Home app warns that it's an uncertified accessory: choose Add Anyway. It needs a home hub.

`oikade pair matter` opens the window again, with no restart, and prints the codes that pair in it:

```console
$ oikade pair matter
Opened the Matter bridge's window until 14:47
Manual code: 34970112332
QR code: MT:Y.K9042C00KA0648G00
Scan it with the Home app, or the camera of any controller.
```

- **While nothing is paired,** that's the bridge's own code.
- **Once something is,** it's a code made for that window, as a controller makes when it shares the bridge, and it pairs nothing once the window closes. The bridge's own code can't add a controller to a paired bridge.
- **Running it again** replaces the window, and its code. A window closes once a controller pairs, or when it times out, and `pair` refuses while a controller is pairing or sharing the bridge.
- **`--json`** prints `{"ok": true, "ref": "matter", "window": …}`, whose `window` has `manualCode`, `qrCode`, `passcode`, `fresh` (a code made for this window) and `expires`.

To add the bridge to a second controller, you can also share it from the first one: in the Home app, the bridge's settings, Turn On Pairing Mode.

### Its network

The host needs host networking on its machine: the same L2 network as your controllers, with mDNS and IPv6.

- **`--matter-port`** moves the bridge from UDP 5540, and 0 turns Matter off.
- **`--matter-interfaces`** limits where it advertises, as a comma-separated list. Empty is every multicast interface.
- **If its port is taken, or an interface isn't there,** the host logs that and carries on without the bridge, and tries again every minute. `oikade status matter` says it's down and why. Automations don't need it.

### Naming the bridge

Your home names the bridge, as a controller first shows it. It's `Oikade` until a deploy says otherwise:

```go
home := oikade.NewHome(oikade.Options{MatterName: "Cottage"}) // at most 32 bytes
```

A deploy renames it in place, and the host keeps the name through a restart, so you don't pair again. The bridge's Basic Information says the new name at once, and the next commissioning window advertises it. Whether a controller that's already paired, such as the Home app, shows the new name isn't verified: it may keep the one it was given.

### What's on the bridge

Every device on record with something Matter can show is on the bridge, at its own endpoint, as soon as it's paired, as its [device types](#what-a-device-is). That's unless your home [hides its plugin's devices](#hiding-a-plugins-devices-from-matter).

- **Its label is its name.** A rename changes that in place: the endpoint and unique ID stay, so a controller keeps its room and scenes.
- **A new device type** rebuilds the device on the same endpoint, with the same unique ID.
- **Endpoint IDs are never reused or reordered,** and forgotten devices keep theirs.
- **Forgetting a device** takes it off. A device whose plugin hasn't reported yet stays on, unreachable, so controllers don't see it vanish.
- **A device none of whose capabilities map to Matter,** such as one with only `link-quality`, keeps its endpoint ID but isn't on the bridge.

### Writes from a controller

A controller's write goes through [arbitration](automations.md#arbitration) in the host, as `matter`, and doesn't need your process.

- **A capability a Safety claim holds refuses it,** including a claim still on disk from before a restart. The write never reaches the device, the controller sees the value go back, and the log says who holds it.
- **A device whose claim record can't be read counts as held at Safety:** nothing writes it, nothing writes over the record, and the log says so. `oikade forget --allow-safety` clears it.
- **A burst of commands,** such as a swipe in the Home app, is gathered for 50 ms per device and capability, and only the last is written, unless the device is already in that state. A blind's or a shutter's position gathers until 1.5 s after the last command, so one drag of the Home app's slider is one move. A stop isn't held back.

### Hiding a plugin's devices from Matter

Some platforms put their devices in the Home app themselves, as a TaHoma hub does its shutters. Oikade still drives them through its plugin, but on its bridge too they'd show in the Home app twice, in Siri and in every scene. Your home keeps them off Matter where it imports the plugin:

```go
home.Plugin("tahoma", overkiz.New(overkiz.Local{ /* … */ })).
	HideFromMatter().            // the TaHoma app puts these in the Home app itself
	ShowOnMatter("garage-door")  // except this one, which it doesn't
```

- **Your code still reads and sets them,** and a tap on the platform's own tile reaches Oikade as the device changing by itself, as its own remote's would.
- **A device the plugin reports later** is hidden too.
- **A deploy hides them once the version has settled,** so one that fails leaves the bridge as it was. The host keeps the setting before it applies it, so a restart keeps them off before your process connects.
- **`oikade check` says which devices a deploy would take off,** and fails on a `ShowOnMatter` name that isn't one of that plugin's devices on record. `oikade deploy` says which it took off.
- **A controller loses a device taken off,** with its room and scenes there. A hidden device keeps its endpoint, so showing it again brings it back as the same device to Matter, though whether the Home app puts it back in its room isn't verified.

`oikade list devices` shows a hidden device as `hidden from Matter`, and `--json` as `"matter": false`.
