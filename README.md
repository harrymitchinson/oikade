# Oikade

Home automation as reconciled, reviewable infrastructure.

You declare your home in Git: devices, rooms, rules and the Matter bridge. Kubernetes reconciles it. Live state never touches etcd. It travels over MQTT, and Prometheus keeps its history.

| | Owner | Examples |
|---|---|---|
| **Control plane** | Git, as CRDs | Which devices exist, their names and rooms, rules, the Matter bridge |
| **Data plane** | MQTT (Mosquitto) | "the light is on", "motion detected", "21.4°C", "turn the light off" |
| **History** | Prometheus and Grafana | Temperature over time, how often the door opened, rule runs |

Toggling a light from your phone is state, so it goes over the bus and Git doesn't care. Pairing a bulb that isn't declared in Git is drift, and Oikade reports it.

## Components

```
 drivers                           bus                        consumers
 ┌──────────────┐                                      ┌──────────────────────┐
 │ zigbee2mqtt  │◀──────▶  Mosquitto (MQTT)  ◀────────▶│ runtime              │ → Device status, /metrics, rules
 ├──────────────┤             ▲                    └──▶│ Matter bridge (per   │ → the Home app, Google Home, …
 │ plugins      │◀────────────┘                        │   Bridge)            │
 └──────────────┘                                      └──────────────────────┘
        ▲
        └── operator: renders configuration.yaml and devices.json from Devices
```

| Kind | Group | Purpose |
|------|-------|---------|
| `Device` | `home.oikade.dev` | A device that should exist: name, room and driver source (a Zigbee IEEE address, or a Plugin and a plugin-local id), and which capabilities it arbitrates. Status shows its capabilities, availability and claims. |
| `Room` | `home.oikade.dev` | Labels metrics and groups devices. Matter controllers such as the Home app don't take rooms from a bridge, so you also assign them once there. |
| `Rule` | `home.oikade.dev` | A moment (`triggers`, a CEL condition and `do`) or a hold (`while` and `hold`). See [Rules](#rules). |
| `Zigbee2MQTT` | `zigbee.oikade.dev` | A Zigbee2MQTT instance the operator configures and runs. |
| `Plugin` | `plugins.oikade.dev` | A plugin process the operator runs for a device platform Oikade doesn't speak natively. See [Plugins](#plugins). |
| `Bridge` | `matter.oikade.dev` | A Matter bridge that exposes a set of Devices to controllers such as the Home app, one bridged endpoint each. See [Matter bridge](#matter-bridge). |

Illustrative output:

```console
$ kubectl -n home get devices
NAME             NAME             ROOM      CAPABILITIES                                  AVAILABLE   LAST SEEN
hallway-light    Hallway Light    hallway   ["light.on","light.level","link.quality"]     True        2m
hallway-motion   Hallway Motion   hallway   ["sensor.occupancy-detected",...]             True        1m
```

There is one binary with three subcommands:

- **`manager`** is the operator. It only talks to Kubernetes.
- **`manager runtime`** is the data plane. It follows every driver on the bus, writes coarse Device status (capabilities, availability, and `lastSeen` at most every 5 minutes), serves Prometheus metrics on `:8080` and runs rules. It runs as one replica, because two would run every rule twice.
- **`manager matter --bridge <name>`** is a Matter bridge, started by the operator for each `Bridge`. It follows the bus itself, so controllers keep working if the runtime is down. It never writes to the API. See [Matter bridge](#matter-bridge).

## Requirements

- **The Matter bridge pod's node must be on the same L2 network as your controllers, with IPv6.** Controllers find the bridge over mDNS and reach it over IPv6, so the pod uses `hostNetwork: true`. Kind or minikube on macOS run inside a VM, where the Home app can't see them. Use those for development, and a real Linux node (k3s, Talos, etc.) for pairing.
- **A Matter controller with a hub,** such as the Home app with an Apple TV or HomePod as its home hub.
- **A Zigbee coordinator.** A network coordinator (`tcp://host:6638`, e.g. SLZB-06) lets the Zigbee2MQTT pod run anywhere. A USB stick (`/dev/ttyUSB0`) needs `nodeSelector` to pin the pod to that node, and the pod then runs privileged.
- A default StorageClass.

## Quick start

For a full walkthrough on real hardware, from blank Pis to a paired bridge, with GitOps, monitoring and backups, see [Deploying on a Raspberry Pi cluster](docs/deploy-raspberry-pi.md).

```sh
make docker-build docker-push IMG=<registry>/oikade:dev
make deploy IMG=<registry>/oikade:dev      # operator, runtime and Mosquitto in oikade-system

kubectl create namespace home
# Edit config/samples first: the coordinator address and IEEE addresses.
kubectl -n home apply -k config/samples
kubectl -n home get zigbee2mqtts,devices,bridges,rules
```

1. **Pair devices.** Open the Zigbee2MQTT frontend (`kubectl -n home port-forward svc/z2m 8080`), permit joining, and pair. A paired device that isn't declared shows up in `oikade_undeclared_devices` and the runtime logs.
2. **Declare them.** Add a `Device` for each IEEE address. The operator sets each device's Zigbee2MQTT friendly name to the Device name.
3. **Pair the bridge.** Read its pairing code from its Secret (see [Matter bridge](#matter-bridge)). In the Home app, choose **Add Accessory**, then **More options**, then the bridge, and enter the code.
4. **Set your timezone and location.** Edit `--timezone` in `config/runtime/deployment.yaml` so rules see local time, and `--latitude` and `--longitude` if you use sun triggers.

## Rules

A `Rule` has one of two shapes ([OIK-64](https://linear.app/oikade/issue/OIK-64)):

- **Moments** (`triggers`) do something when a trigger fires: a capability changes, a time of day comes, or the sun rises or sets.
- **Holds** (`while`) keep a capability at a value for as long as a window or a condition holds. The runtime works out from the clock and the devices' state whether a hold is active, so it stays right across a restart.

Capabilities are last-write-wins unless the Device **arbitrates** them with a [`control`](#arbitration) entry. Then rules, Matter controllers and the device itself make claims, the device takes the winner, and `kubectl get device` says why.

The Kind e2e applies every complete manifest in this section, and expects each Rule and Device to be `Ready`.

### Moments

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: hallway-motion-light}
spec:
  mode: Restart          # new motion restarts the 5 minutes; Single ignores triggers while running
  triggers:
  - change: {device: hallway-motion, capability: sensor.occupancy-detected}
  condition: trigger.value && (now.getHours(tz) >= 18 || now.getHours(tz) < 7)
  do:
  - set: {device: hallway-light, capability: light.on, value: true}
  - delay: 5m
  - set: {device: hallway-light, capability: light.on, value: false}
```

The condition is [CEL](https://cel.dev). It can use these variables:

| Variable | Type | |
|---|---|---|
| `trigger` | map | `type`: `change`, `at` or `sun`. For a change also `device`, `capability`, `value` and `previous` (null the first time) |
| `devices` | map | Device name → capability → current value, for every Device in the namespace |
| `now` | timestamp | |
| `tz` | string | The runtime's `--timezone`. CEL's accessors default to UTC, so write `now.getHours(tz)`. |

The 5 minutes in this example live in the runtime's memory, so a restart during them leaves the light on. The [hold](#holds) version below doesn't have that problem.

### Time and sun triggers

A trigger can be a time of day (`at`) or sunrise or sunset (`sun`) instead of a `change`:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: downstairs-bedtime}
spec:
  triggers:
  - at: {time: "23:30", days: [Sun, Mon, Tue, Wed, Thu]}   # weeknights
  - at: {time: "00:30", days: [Sat, Sun]}                  # Friday and Saturday nights
  - sun: {event: Sunset, offset: -15m}                     # 15 minutes before sunset
  startingDeadline: 10m
  do:
  - set: {device: porch-light, capability: light.on, value: false}
```

- **Times are local,** in the runtime's `--timezone`. A time the clocks skip when daylight saving starts fires when they go forward, and a time that happens twice when it ends fires the first time.
- **`days` is the calendar day the trigger fires on.** 00:30 after Friday evening is Saturday, so Friday and Saturday nights at 00:30 are `days: [Sat, Sun]`. Leave `days` out for every day.
- **Sun times** are computed by the runtime from `--latitude` and `--longitude`, to within about a minute. `offset` is between `-6h` and `6h`. Without a location, sun triggers don't fire, and the Rule shows `Scheduled=False` with reason `NoLocation`. Where the sun doesn't rise or set that day, its trigger doesn't fire that day.
- **`startingDeadline`** is how late a time may still fire after the runtime was down at it. A later one is skipped, logged and counted as `missed`. Without `startingDeadline`, every missed time is skipped. Only the latest missed time counts, and a new Rule never fires for times before it was created.
- `status.nextFire` answers "will the porch light come on tonight?". `status.lastFire` is the last time fired or skipped, so a restart doesn't fire it again.

### Rooms

A `change` trigger, a `set` action or a `hold` can name a `room` instead of a `device`, but not both. A Device is in a room when its `roomRef` names it, so a lamp added to the room later is covered without editing the Rule.

```yaml
spec:
  triggers:
  - change: {room: hallway, capability: sensor.occupancy-detected}   # any sensor in the hallway
  do:
  - set: {room: living-room, capability: light.on, value: false}   # every light in the living room
```

- **Room triggers** fire on a change from any Device in the room at the time of the change. `trigger.device` is still the Device that changed.
- **Room actions** set every Device in the room that has reported the capability, in name order, and skip the rest. So `light.on` on a room leaves the motion sensor and the thermostat alone, and a Device that hasn't been discovered yet is skipped. Membership is read when the action runs, so a lamp added to the room during a `delay` is included in the actions after it.
- **If some Devices fail**, the others are still set and the remaining actions still run, so a dead bulb can't leave the rest of the room on. The run counts as `failed`. A room with nothing to set counts as `succeeded`, and the runtime logs `Matched no Devices in Room` with `--log-level=debug`.
- **Restart mode with a room trigger** restarts the run on a change from any sensor in the room, which suits people moving between sensors. Drivers only report real changes, so someone standing still in front of one sensor doesn't restart the run. Keep the `delay` longer than the time your sensor holds `occupancy: true` after the last movement.
- **A room `set` with no end skips arbitrated members,** because a claim must end (see [one-off claims](#one-off-claims)). The Rule lists them in `status.skipped`.

### Holds

A hold keeps a capability at `hold.value` while everything in `while` is true:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: water-heater-night}
spec:
  while:
    window: {start: "22:00", end: "06:00"}   # an end at or before the start is the next morning
  hold: {device: water-heater, capability: switch.on, value: true}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: bathroom-humid}
spec:
  while:
    when: devices["bathroom-sensor"]["sensor.relative-humidity"] > 70
    offDelay: 10m                            # run on for 10 minutes after it drops
  hold: {device: bathroom-fan, capability: switch.on, value: true}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: porch-evening}
spec:
  while:
    window: {start: {sun: Sunset, offset: -15m}, end: "23:30"}
  hold: {device: porch-light, capability: light.on, value: true}
```

| Field | |
|---|---|
| `window` | `start` and `end` are quoted `"HH:MM"` local times, or `{sun: Sunrise\|Sunset, offset}`. `days` limits it to windows that start on those days. |
| `when` | CEL over `devices`, `now` and `tz`, as in a condition. |
| `onDelay` | How long the rest must stay true before the hold starts. |
| `offDelay` | How long the hold stays active after the rest stops being true. |
| `latch` | Stays active once `when` has been true, until a `release`. See [below](#latched-safety-holds). |

A `while` needs a `window`, a `when` or a `budget`, and everything in it must be true together. A `budget` is the only field of its `while`.

A `budget` runs a device for a number of minutes a day instead, in the slots that suit it best. See [Budgets](#budgets).

- **When a hold starts,** the runtime writes `hold.value`. When it ends, a bool hold writes the opposite, and a number hold writes nothing, so the device keeps the held value. To go back to a set value, give the capability a `control` entry: its `default` is what's left when the hold ends.
- **Without a `control` entry, the last write wins.** A Home app tap at 23:00 turns the water heater off until the hold next starts.
- **Restarts are safe.** The runtime saves each hold's state, and on startup it writes only if the hold started or ended while it was down. A restart at 02:00 doesn't undo the 23:00 tap, but a runtime down from 21:50 to 02:00 turns the heater on when it comes back. `offDelay` deadlines are saved too, so the fan still stops on time.
- `status.active` and `status.nextChange` say whether the hold is active and when that next changes. `kubectl get rules` shows `ACTIVE`.
- **Two holds on one capability need a `control` entry** to decide between them. Without one, both Rules show `Ready=False` with reason `ConflictingHolds`.

### Budgets

A budget hold runs a device for a number of minutes a day, and plans when. Here a loft fan runs one to five hours a day depending on how humid the loft was at midnight, overnight where it can, and never in the evening:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: loft-fan}
spec:
  zigbee2mqtt: {instanceRef: {name: z2m}, ieeeAddress: "0x00158d00000000f1"}
  control:
  - capability: switch.on
    default: false
    resolve: PreferOn
    external: Override
    overrideHold: 1h
    minimumOn: 1h          # each run at least an hour
    minimumOff: 30m        # and half an hour's rest between runs
    maximumStartsPerDay: 3
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: loft-airing}
spec:
  while:
    budget:
      planningTime: "00:00"
      slotMinutes: 30
      minutes:
        bands:
          input: {device: loft-humidity, capability: sensor.relative-humidity, maxAge: 1h}
          missingMinutes: 180   # if the sensor hasn't reported in the last hour
          ranges:
          - {below: 55, minutes: 60}
          - {below: 70, minutes: 180}
          - {minutes: 300}
      prefer:
      - {window: {start: "23:00", end: "07:00"}, score: 3}
      forbid:
      - {window: {start: "18:00", end: "22:00"}}
  hold: {device: loft-fan, capability: switch.on, value: true}
```

| Field | |
|---|---|
| `planningTime` | When each planning day starts, `"HH:MM"` local time. The day is planned then, and the plan is frozen until the next one. Defaults to `"00:00"`. |
| `slotMinutes` | The day is planned in slots of 15, 30 or 60 minutes. Defaults to 30. |
| `minutes.bands` | The day's minutes from the band the `input`'s value falls in at `planningTime`: the first whose `below` it's under, and the last band, with no `below`, for the rest. `missingMinutes` applies when the input hasn't reported within `maxAge` (default 30m). |
| `minutes.expression` | Or CEL over `devices`, `now` and `tz` returning the minutes, such as `devices["loft-humidity"]["sensor.relative-humidity"] > 60.0 ? 240 : 90`. |
| `prefer` | Windows the minutes go in first. Each slot a window touches gains its `score`, from -100 to 100, and overlapping windows add up. |
| `forbid` | Windows the minutes never go in. |

Budget windows are `"HH:MM"` only, not sun times. A budget is the only field of its `while`, and it holds one Device's bool capability at `true`. That capability needs a `control` entry: its `minimumOn`, `minimumOff` and `maximumStartsPerDay` shape the planned runs, and `minimumOn` and `minimumOff` must be whole numbers of slots. Something that must run whatever the budget says, such as frost protection, is a separate `priority: Safety` hold on the same capability, which wins over the budget.

At `planningTime` the runtime plans the day and freezes the plan. The hold is active in the planned blocks, as a Normal claim, so a Safety hold or a Home app tap still beats it. `status.today` shows the plan (`targetMinutes`, `scheduledMinutes`, `shortfallMinutes` and the `blocks`) and the progress: `runMinutes`, counted only while the device reports on and is available and updated at most every 5 minutes, and `starts`. After a restart the same day, the runtime carries on from `status.today` rather than planning again. Editing a budget takes effect at the next `planningTime`. A runtime that starts with no plan for today plans from then, so the hours already gone are lost to the day. The `Planned` condition is `False` with reason `NoControl`, `InvalidBudget` or `EvaluationFailed` when the day can't be planned. `kubectl get rules -o wide` shows each budget's minutes and run.

### Arbitration

A Device arbitrates a capability when it has a `control` entry for it:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: hall-light}
spec:
  zigbee2mqtt: {instanceRef: {name: z2m}, ieeeAddress: "0x000b57fffe0000a1"}
  control:
  - capability: light.on
    default: false          # what's left when nobody claims it
    resolve: PreferOn       # two claims at one level: on wins
    external: Override      # a wall switch or Home app tap becomes a claim...
    overrideHold: Next      # ...until what the rules want next changes
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: hall-motion-light}
spec:
  while:
    when: devices["hall-motion"]["sensor.occupancy-detected"]
    offDelay: 5m
  hold: {device: hall-light, capability: light.on, value: true}
```

Rules, Matter controllers and the device itself make **claims**. The highest level with a claim wins:

| Level | Who claims | Ends |
|---|---|---|
| Safety | Rules with `priority: Safety` | A Rule's claim ends when its hold ends (for a latched hold, at a `release`), or at the set's `for`/`until` |
| Override | Outside changes (a Matter controller, or the device by itself), and Rules with `priority: Override` | An outside change ends after `overrideHold`. A Rule's claim ends as above. |
| Normal | Ordinary Rules | As above |
| Default | Nobody | Never |

| Field | |
|---|---|
| `default` | Required. The value when nothing claims the capability. |
| `resolve` | Required. Which claim wins within a level: `PreferOn` or `PreferOff` for bools, `Highest` or `Lowest` for numbers. Rules have no numeric priorities. |
| `external` | Required. What an outside change does. `Override` makes it a claim for `overrideHold`. `Revert` writes the winner straight back, as for a valve that must stay closed. |
| `overrideHold` | With `Override`: a duration such as `1h`, or `Next`, which ends the override when the winner without it next changes. |
| `minimumOn`, `maximumOn`, `minimumOff`, `maximumStartsPerDay` | Equipment limits. They apply after the winner is picked and beat every Rule, Safety included. An outside change is accepted and counted, but not fought. |

In this example, motion turns the light on, and it goes off 5 minutes after the motion stops. A wall switch turned off during that is an Override, so the light stays off until the motion hold ends. A wall switch turned on with nobody about lasts until the next motion starts, and the light then goes off 5 minutes after the motion stops. The motion hold's off-delay is saved, so a restart doesn't leave the light on.

An outside change is recorded by the path it came in by: `Matter` for a command through a Matter bridge, such as a Home app tap, and `Device` for anything else, such as a wall switch. A Rule's claim is `Rule/<name>`. Oikade can't tell what a person did, so nothing is called "manual".

### One-off claims

A `set` on an arbitrated capability is a claim, so it must say when it ends:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: towel-rail}
spec:
  zigbee2mqtt: {instanceRef: {name: z2m}, ieeeAddress: "0x000b57fffe0000a2"}
  control:
  - {capability: switch.on, default: false, resolve: PreferOn, external: Override, overrideHold: 1h}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: towel-rail-boost}
spec:
  triggers:
  - at: {time: "21:30", days: [Sun]}
  do:
  - set: {device: towel-rail, capability: switch.on, value: true, for: 1h, until: Next}
```

- `for: <duration>` ends it after that long.
- `until: Next` ends it when the winner without it next changes. If the rail also had a hold from 06:00 to 08:00, that's when the hold starts the next morning.
- `for` together with `until: Next` ends it at whichever comes first. This boost ends after an hour, unless what the other claims want changes sooner.
- `until: Released` ends it at a `release: {rule: <name>}` action in another Rule.
- `priority: Safety` or `Override` on the `set` raises the claim's level. It's `Normal` otherwise.

A `set` on an arbitrated capability of a single device with no `for` or `until` sets `Ready=False` with reason `UnendedClaim`. A `set` on a capability that isn't arbitrated is a plain write, and `for` and `until` don't apply to it.

### Latched Safety holds

A latched hold stays active once `when` has been true, until another Rule releases it. It's worked out from state rather than from a change. So if the garage overheated while the runtime was down and is still too hot when the runtime next sees its temperature, it latches then. That's at startup for a plugin device, which retains its state, and at the sensor's next report for a Zigbee2MQTT one:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: garage-heater}
spec:
  zigbee2mqtt: {instanceRef: {name: z2m}, ieeeAddress: "0x000b57fffe0000a3"}
  control:
  - {capability: switch.on, default: true, resolve: PreferOff, external: Revert}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: garage-overheat}
spec:
  priority: Safety
  while:
    when: devices["garage-temperature"]["sensor.temperature"] > 40
    latch: true
  hold: {device: garage-heater, capability: switch.on, value: false}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: garage-overheat-reset}
spec:
  triggers:
  - change: {device: garage-door, capability: sensor.contact-open}
  condition: trigger.value
  do:
  - release: {rule: garage-overheat}
```

Overheating turns the heater off. Turning it on from the Home app is reverted at once, and the heater stays off after the garage cools, until someone opens the door to check. Rules run only while the runtime is up. If the garage overheats and cools again while the runtime is down, nothing latches. Editing the hold's device, room, capability or value starts it afresh, and drops the latch. A `release` naming a Rule that doesn't exist, or that has nothing to release, sets `Ready=False` with reason `InvalidRelease`.

### Reading Device status

`Device.status.control` lists each arbitrated capability's value, the level that won, and every claim:

```console
$ kubectl -n home get device hall-light -o jsonpath='{.status.control}' | jq
[
  {
    "capability": "light.on",
    "value": false,
    "level": "Override",
    "claims": [
      {"source": "Rule/hall-motion-light", "level": "Normal", "value": true},
      {"source": "Device", "level": "Override", "value": false, "winner": true}
    ]
  }
]
```

A claim also shows `expires` when it ends at a set time, `latched` for a latched hold, and `reverted` for an outside change that `Revert` wrote back. Status changes when a claim starts or ends, not on every value. `oikade_device_control_level` has the winning level over time.

### When a Rule isn't Ready

The operator compiles every Rule and sets `Ready=False` with the first problem it finds, in this order:

- `Invalid` when the Rule doesn't compile: a bad condition or `when`, an unknown or read-only capability, or a value of the wrong type.
- `RoomNotFound` when a trigger or action names a Room that doesn't exist in the namespace. The message lists the missing Rooms. Creating them sets `Ready=True` without editing the Rule. The runtime doesn't check Rooms: until the Room exists it still runs the Rule, matching any Devices whose `roomRef` names it. For a misspelled room that's none. After a Room is deleted, Devices still pointing at it keep matching.
- `UnendedClaim`, `ConflictingHolds` and `InvalidRelease`, as above. These follow the Devices and other Rules, so adding a `control` entry or the missing Rule clears them without editing this one.
- `NoControl` when a budget's Device doesn't exist or has no `control` entry for the capability, and `InvalidBudget` when the budget doesn't fit it, such as a `minimumOn` that isn't a whole number of slots, or bands out of order.

A Device whose `control` entry doesn't fit its capability, such as `resolve: Highest` on a bool, shows `Ready=False` with reason `InvalidControl`, and that capability stays last-write-wins. `status.lastTriggered` is updated at most once a minute.

### Migrating from Automation

`Rule` replaces the `Automation` kind, with the same behaviour ([OIK-64](https://linear.app/oikade/issue/OIK-64)). To move an Automation over:

- Change `kind: Automation` to `kind: Rule`.
- Wrap each entry of `triggers` in `change:`. For example, `- {device: m, capability: c}` becomes `- change: {device: m, capability: c}`.
- Rename `actions` to `do`. `condition`, `mode`, `delay` and room targets are unchanged.
- In PromQL, `oikade_automation_runs_total{automation=…}` is now `oikade_rule_runs_total{rule=…}`, and `oikade_automation_events_dropped_total` is now `oikade_rule_events_dropped_total`.

Apply the Rules, then delete the old Automation CRD with `kubectl delete crd automations.home.oikade.dev`. That also deletes every Automation, which the runtime stops running.

## Capabilities

Drivers translate to and from a protocol-neutral set of capabilities. Matter, metrics and rules only ever see these. A Device with several sensors is one Matter endpoint with several device types, and a battery is a Power Source on it.

| Capability | Type | Writable | Matter |
|---|---|---|---|
| `light.on` | bool | ✓ | On/Off Light, or Dimmable Light with `light.level` |
| `light.level` | 0–100 | ✓ | Level Control on a Dimmable Light |
| `switch.on` | bool | ✓ | On/Off Plug-in Unit, or as `spec.matter.deviceType` says |
| `outlet.on` | bool | ✓ | On/Off Plug-in Unit |
| `sensor.temperature` | °C | | Temperature Sensor |
| `sensor.relative-humidity` | % | | Humidity Sensor |
| `sensor.contact-open` | bool | | Contact Sensor (Boolean State, `true` for closed). `true` means open, the inverse of Zigbee2MQTT's `contact`. |
| `sensor.occupancy-detected` | bool | | Occupancy Sensor (passive infrared) |
| `battery.level` | % | | Power Source, with a warning below 20% |
| `link.quality` | 0–255 | | (metrics only) |

## Plugins

A plugin connects a device platform that isn't Zigbee, such as the eWeLink LAN protocol or the Overkiz cloud API, without changing Oikade. It targets the platform, not one device: a new device on that platform is a model in the plugin, not a new plugin. It's a separate process that speaks Oikade's capabilities over MQTT, following the [oikade/v1 plugin contract](docs/plugin-contract.md). Devices it serves are ordinary `Device`s: they show up on the Matter bridge, in Grafana and in rules like Zigbee ones, and keep working through the bridge while the runtime is down.

```yaml
apiVersion: plugins.oikade.dev/v1alpha1
kind: Plugin
metadata: {name: ewelink-lan}
spec:
  image: ghcr.io/harrymitchinson/oikade-plugin-ewelink-lan:v0.1.0   # illustrative: each plugin is released from its own repo
  hostNetwork: true              # for plugins that need mDNS or multicast
  secretRef: {name: ewelink-lan-keys} # mounted at /etc/oikade/secrets, one key per device id
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: porch-light}
spec:
  roomRef: {name: porch}
  plugin:
    pluginRef: {name: ewelink-lan}
    id: 1000aabbcc               # plugin-local and stable
    parameters: {capability: light.on}   # passed through; the plugin validates them
```

The operator runs each Plugin as a one-replica Deployment `<plugin>-plugin`, and writes the Devices declared for it to `devices.json` in the Secret `<plugin>-plugin-config`. Any change to the Devices it serves restarts the pod: adding or removing one, or changing its id, name, display name, room or parameters. So does a change to the `secretRef` Secret. Plugins are trusted images you build: they get no service account token and run as non-root with a read-only root filesystem.

The Plugin's `Ready` condition is `True` with reason `Available` once the pod runs and nothing below applies. Otherwise it's `False` with one of these reasons, and `DuplicateDevice`, `SecretNotFound` and `NameConflict` show even while the pod is running:

- `Progressing`: the pod isn't available yet
- `SecretNotFound`: the `secretRef` Secret doesn't exist. The plugin still runs and reports an error for each device that needs a key.
- `DuplicateDevice`: two Devices claim the same `id`. The first by name wins.
- `NameConflict`: `<plugin>-plugin` or `<plugin>-plugin-config` exists and belongs to something else, or `secretRef` names `<plugin>-plugin-config`

A plugin reports problems with a device, such as bad parameters or a missing key, as `Resolved=False` with reason `PluginError` on the Device. Messages Oikade can't validate are dropped and counted in `oikade_plugin_rejected_messages_total`.

To write a plugin in Go, use the [SDK](sdk/plugin) and the contract types in [`contract/v1`](contract/v1). A plugin's whole `main` is one `plugin.Run` call, and the SDK handles the Last Will, re-announcing after a reconnect, `/set` and SIGTERM. Each plugin lives in its own repo, `oikade-plugin-<name>`, with its own images and releases. [`test/fixtures/simswitch`](test/fixtures/simswitch) is a complete plugin with simulated relays, which this repo's conformance tests drive. A plugin in another language implements the [contract](docs/plugin-contract.md) itself. Any plugin repo can check itself against the contract with [`oikade-conformance`](docs/plugin-contract.md#conformance).

## Metrics

The runtime serves these on `:8080/metrics`. Enable `../prometheus` in `config/default` for a ServiceMonitor, and `../grafana` for the "Oikade home" dashboard.

| Metric | |
|---|---|
| `oikade_capability_value{namespace,device,room,capability}` | Current value. Bools are 0 or 1. |
| `oikade_capability_changes_total{…}` | Counts changes, including ones shorter than the scrape interval (a door opened and closed within 15s) |
| `oikade_device_available{namespace,device,room}` | 1 online, 0 offline |
| `oikade_device_last_seen_timestamp_seconds{…}` | |
| `oikade_undeclared_devices{namespace,instance}` | Paired but not in Git |
| `oikade_plugin_up{namespace,plugin}` | 1 while the plugin reports itself online with contract 1 |
| `oikade_plugin_rejected_messages_total{namespace,plugin,reason}` | Plugin messages dropped as `malformed`, `wrong_contract`, `unknown_capability`, `invalid_value`, `undeclared_device` or `unannounced_capability` |
| `oikade_rule_runs_total{namespace,rule,result}` | `succeeded`, `failed`, `cancelled`, `busy`, `condition_false`, `condition_error`, `missed` |
| `oikade_rule_next_change_timestamp_seconds{namespace,rule}` | When a Rule's next `at` or `sun` trigger fires, or its hold next starts or ends |
| `oikade_rule_active{namespace,rule}` | 1 while a Rule's hold is active, 0 otherwise |
| `oikade_rule_budget_target_minutes{namespace,rule}` | A budget hold's minutes for the current planning day |
| `oikade_rule_budget_scheduled_minutes{namespace,rule}` | How many of them today's plan places |
| `oikade_rule_budget_run_minutes_total{namespace,rule}` | Minutes a budget hold's device has been on and available |
| `oikade_rule_budget_today_run_minutes{namespace,rule}` | Those minutes in the current planning day, as in `status.today` |
| `oikade_rule_budget_desired{namespace,rule}` | 1 while a budget hold's plan wants its device on, 0 otherwise |
| `oikade_device_control_level{namespace,device,capability}` | The level that wins an arbitrated capability: 0 default, 1 Normal, 2 Override, 3 Safety |
| `oikade_rule_events_dropped_total` | Capability changes the rule engine dropped because it fell behind |

Every capability change is also logged as `"Changed capability"` with structured fields, so Loki has the event history.

## Matter bridge

Oikade reaches the Home app and other controllers through Matter ([OIK-76](https://linear.app/oikade/issue/OIK-76)). The operator runs **`manager matter`** for each `matter.oikade.dev` `Bridge`: one pod with `hostNetwork`, which controllers find over mDNS and reach over IPv6 on a UDP port allocated from `spec.portRange` (5540 to 5640 by default). It follows the bus itself, so controllers keep working while the runtime is down, and only reads the API. Each selected Device with something Matter can show is a bridged endpoint, mapped as in [Capabilities](#capabilities).

```console
$ kubectl get bridges
NAME   PORT   DEVICES   READY   AGE
main   5540   4         True    2m
```

The pairing codes are in the bridge's `<bridge>-matter` Secret, behind the same RBAC as its setup passcode:

```sh
kubectl get secret main-matter -o jsonpath='{.data.manual-code}' | base64 -d   # 11 digits
kubectl get secret main-matter -o jsonpath='{.data.qr-code}' | base64 -d       # MT:... for a QR code
```

Add the bridge to a controller, such as the Home app, with the 11-digit manual code, or a QR code of the `MT:` payload. Apple Home warns that it's an uncertified accessory: choose Add Anyway. It needs a home hub. The operator derives both codes from the Secret's `passcode` and `discriminator`, rewrites them if you change those, and removes them while those are invalid.

- **The pairing window** is open for 15 minutes after the bridge starts on no fabric. If it closes first, restart the pod: `kubectl rollout restart deploy/<bridge>-matter`. To add the bridge to a second controller, open a window from the first one (in the Home app: the bridge's settings, Turn On Pairing Mode).
- **Endpoints:** `status.endpoints` records the endpoint ID each Device was given. IDs start at 2, are handed out in name order and are never reused or given to another Device, and the API rejects a status that breaks that.
- **Fabrics:** `status.fabrics` lists the controllers it's commissioned into: one per ecosystem, such as Apple Home. The bridge pod publishes them on `oikade/v1/_matter/<namespace>/<bridge>/fabrics` (retained), and the runtime copies them into status, so they update only while the runtime runs.
- **Writes:** a command from a controller is written to the device through the bus. A burst of commands, such as a swipe in the Home app, is gathered for 50 ms per device and capability, and only the last is written, unless the device is already in that state. If the capability is [arbitrated](#arbitration), the write is announced on `oikade/v1/_matter/<namespace>/<bridge>/writes` first, and the change becomes a claim from `Matter`.
- **Unsupported Devices:** a Device none of whose capabilities map to Matter, such as one with only `link.quality`, keeps its endpoint ID but isn't on the bridge. The Bridge's `DevicesSupported` condition is False and names it.

`spec.matter.deviceType` says what a `switch.on` Device is shown as: `Light`, `PlugInUnit` (an outlet) or `MountedControl` (a wall switch). Whether it's the on/off or dimmable member of that family follows from the Device's capabilities.

```yaml
spec:
  matter:
    deviceType: PlugInUnit
```

## State that must survive

Losing these means re-pairing, and then the Home app forgets every room, scene and automation, or every Zigbee device has to be paired again.

- **The Matter bridge's Secret** (`<bridge>-matter`: the setup code, unique ID and attestation key, and a copy of the endpoint IDs) and **fabrics** (`<bridge>-matter` PVC: operational keys and certificates, and access control) have no owner reference. Deleting a Bridge keeps them, and a Bridge applied again gets its old pairing code and endpoint IDs back. If the PVC is lost, commission the bridge again.
- **Matter endpoint IDs** live in `Bridge.status.endpoints`, with a copy in the Secret. They are only ever appended: never reused or reordered, and kept for deleted Devices, so a Device that comes back gets its old ID.
- **The Zigbee network key** (`<instance>-network` Secret) and **database** (`<instance>-data` PVC) have no owner reference either. Back up the Secret.
- **Zigbee2MQTT's `configuration.yaml` is overwritten on every start** with the version rendered from Git. Changes made in its frontend don't survive a restart.
- **The runtime's state** lives in retained MQTT messages under `oikade/v1/_runtime/`, kept on Mosquitto's `oikade-mosquitto` PVC: which holds are active, latches, off-delay deadlines, claims and their expiries, and equipment-limit counters. Losing it is safe but noticeable. Holds and claims are worked out again from the clock and the devices, but latched holds whose condition has passed are released, overrides end early, and limits start counting again.
- **A plugin's credentials** (the `spec.secretRef` Secret, such as LAN keys) belong to you. The operator never creates, changes or deletes them, so keep them in Git, encrypted with SOPS or similar. Everything else a plugin needs is rendered from Git, and plugins keep no state of their own.

To really forget a Bridge or Zigbee2MQTT instance, delete those objects yourself:

```sh
kubectl delete secret,pvc -l matter.oikade.dev/bridge=<name>
kubectl delete secret,pvc -l zigbee.oikade.dev/instance=<name>
```

## Security notes

- Mosquitto allows anonymous access inside the cluster. Anything that can reach it can control your devices. Restrict it with a NetworkPolicy, or bring your own broker and set `--mqtt-url` on the operator, runtime and Bridge pods.
- Matter bridge pods use the host network, run as non-root and only have read access to the API, with no access to Secrets: the operator mounts each one's own setup code and attestation key.
- Plugins publish on the same anonymous broker, so anything on the cluster network could also publish as a plugin, just as it could as Zigbee2MQTT.
- Matter bridges announce each write on `oikade/v1/_matter/`, so status can say where a change came from. Anything on the broker could publish a false announcement. That only changes the source shown on a claim, not what Oikade does. It could also publish false fabrics, which only changes what `status.fabrics` shows.

## Logging

The operator, `runtime` and `matter` commands write structured JSON logs to stderr using
Go's `log/slog`, including controller-runtime logs. Use `--log-level=debug|info|warn|error`
to set the minimum level (default: `info`). Errors are always logged.
The former `--zap-*` flags are no longer supported.

## Versioning

Releases follow [semantic versioning](https://semver.org), and Git tags and image tags use the same version, for example `v0.1.0`. Oikade's public API is the CRDs, capability names, CEL variables, metric names and labels, and Matter endpoint IDs.

While the major version is 0, the API is still settling, and versions follow the milestones, which are the releases in the plan (`docs/planning.md`):

- Releases on the way to a milestone are pre-releases of it: `v0.1.0-alpha.1`, `v0.1.0-alpha.2`, … Any of them may break the public API, so read the release notes before upgrading.
- Shipping a milestone cuts its version (`v0.1.0`). Releases after it are `v0.2.0-alpha.N`, on the way to the next one.
- No commit moves MINOR or MAJOR on its own, breaking changes included.

`1.0.0` is a deliberate decision, not something a commit triggers.

Releases are cut by merging the release PR that [release-please](https://github.com/googleapis/release-please) keeps open on `main`. That gives the tag, a GitHub Release with notes and `install.yaml`, and the image `ghcr.io/harrymitchinson/oikade:<tag>` for arm64 and amd64. CI doesn't run on the release PR, because it's opened with `GITHUB_TOKEN`. That's fine, since it only changes `CHANGELOG.md` and `.release-please-manifest.json`. release-please needs **Settings → Actions → General → Allow GitHub Actions to create and approve pull requests** turned on. The first release creates the ghcr package as private, so set its visibility to public once, in the package's settings.

To ship a milestone:

1. Merge a commit whose message ends with a `Release-As: 0.1.0` footer, as its last paragraph. With a squash merge, put it last in the squashed message.
2. Merge the release PR for `v0.1.0`.
3. Before anything else merges, merge a commit with a `Release-As: 0.2.0-alpha.1` footer. Otherwise release-please's prerelease versioning proposes `v0.2.0-alpha` after a feature, or `v0.1.1-alpha` after a fix, rather than the next milestone's first alpha.

## Development

```sh
make test          # unit tests, envtest, and an in-process MQTT broker for the runtime integration test
make lint
make test-e2e      # Kind: fakes Zigbee2MQTT and a plugin over MQTT and checks status, rules, holds, claims, metrics, and commissions and drives the Matter bridge with chip-tool (`test/e2e/chip-tool`, linux/arm64 only)

make test-conformance   # builds the simswitch test fixture and drives it through the plugin contract
```

The plugin SDK, the conformance suite and the simswitch fixture are Go, in the same module, so `make test` and `make lint` cover them. How the components, rule engine and Matter bridge work inside is in [docs/architecture.md](docs/architecture.md).

## Not yet

Planned work is tracked in [Linear](https://linear.app/oikade). Each release is a project there, and its description gives its goal, exit criteria and why it sits where it does. Designs live in their features' issues there, and [docs/planning.md](docs/planning.md) says how planning works.

## Not doing

Oikade isn't a general-purpose hub. It has no Home Assistant driver and no UI for configuration.

- **Home Assistant as a driver.** Standalone device libraries that Home Assistant also uses are fine inside plugins.
- **Runtime replicas or leader election.** A single replica is deliberate, because two would run every automation twice.
- **Homebridge as a driver.** It would make Homebridge, rather than Git, the source of device identity ([OIK-61](https://linear.app/oikade/issue/OIK-61)).
- **Go `plugin` or WASM plugins.** The first needs CGO, and neither can do LAN multicast usefully.
- **Configuration from a UI.** Changes made in the zigbee2mqtt frontend count as drift, by design.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
