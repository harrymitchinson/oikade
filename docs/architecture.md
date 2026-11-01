# Architecture

How Oikade works inside, as it is now. It's for anyone changing the code. What a user writes and sees is in the [README](../README.md), and the plugin protocol is in [plugin-contract.md](plugin-contract.md).

This page says how things work, not why they were chosen. Each section links the Linear issue that holds the design and its discussion. When a change makes a section wrong, the same PR fixes it.

## Planes

| Plane | Owner | Holds |
|---|---|---|
| Control | Git, as CRDs (`home.oikade.dev`, `homekit.oikade.dev`, `zigbee.oikade.dev`, `plugins.oikade.dev`) | What should exist: Devices, Rooms, Rules, Bridges, Zigbee2MQTT instances and Plugins |
| Data | MQTT (Mosquitto) | Live state and commands |
| History | Prometheus, Grafana and Loki | Values over time, changes and rule runs |

Live state never goes into etcd. `Device.status` is deliberately coarse: capabilities, conditions, claims at their transitions, and `lastSeen` at most every 5 minutes. Git owns names: the operator renders each driver's configuration from Devices and overwrites it on every start. Something that exists but isn't declared, such as a paired but undeclared device, is drift: a metric and a log line, never adopted.

## Components

One binary, `cmd/`, has three roles:

| Role | Talks to | Runs |
|---|---|---|
| `manager` | Kubernetes only | The controllers in `internal/controller/<group>`, which validate objects and render what they manage with `internal/render` |
| `manager runtime` | MQTT, and the API for status | One replica. The drivers, the hub, the status writer, `/metrics` on `:8080`, and the rule engine with arbitration |
| `manager hap --bridge <name>` | MQTT, and the API read-only | One per Bridge, with `hostNetwork`. A HomeKit accessory server for that Bridge's Devices |

The runtime is a single replica because two would run every rule twice. Anything that assumes it's highly available, or needs leader election, is a design decision. hap follows the bus itself, so HomeKit keeps working while the runtime is down. It can read only its own Secrets.

### The data path

```
driver ──▶ hub ──▶ status writer, metrics exporter, rule engine   (runtime)
  ▲         └────▶ HAP bridge                                     (hap)
  └── Set ◀── engine (through arbitration), HAP
```

- **A driver** translates one integration to capabilities. It reports to a `driver.Sink` (`Discovered`, `Changed`, `Availability`, `Seen`, `Undeclared`) and takes writes as a `driver.Setter`. The drivers are `internal/driver/zigbee2mqtt` and `internal/driver/plugin`. A new kind of integration is a new driver and a new member of the Device spec's driver union, and usually its own API group.
- **The hub** (`internal/hub`) holds the latest state of each device and fans updates out. Both the runtime and hap build one.
- **Capabilities** (`internal/capability`) are the protocol-neutral names and value types every consumer uses. Drivers convert to them, and nothing after the driver knows about vendor payloads.

### Drivers

- **Zigbee2MQTT.** The operator runs each instance and renders its `configuration.yaml` from Devices. The driver maps each device's exposes to capabilities.
- **Plugins** speak the [oikade/v1 contract](plugin-contract.md) under `oikade/v1/<namespace>/<plugin>/`. The operator runs each Plugin as a Deployment and gives it its Devices in `devices.json`. The driver validates every message against the contract and the capability registry, and drops and counts what doesn't fit. The Go SDK is `sdk/plugin`, and the contract types are `contract/v1`. Design: [OIK-61](https://linear.app/oikade/issue/OIK-61) (plugin drivers) and [OIK-62](https://linear.app/oikade/issue/OIK-62) (the Go SDK).

## Rules

The engine (`internal/engine`) loads each Rule the manager has accepted, compiled with its CEL conditions. The manager checks Rules (`internal/controller/home`) and sets `Ready=False` on one it can't run, but only the runtime runs them. Design: [OIK-64](https://linear.app/oikade/issue/OIK-64) (the rule model) and [OIK-60](https://linear.app/oikade/issue/OIK-60) (room targets).

- **Moments** (`triggers`) run `do` when a trigger fires and the condition holds. Time and sun triggers are timers, with sun times from `internal/sun`. A Room target resolves to its members when the trigger or action runs, not when the Rule loads, so a Device added to a room is covered at once.
- **Holds** (`while`) aren't timed in memory. The engine works out from the clock and the devices' state whether each hold is active, and when that next changes. It saves each hold's state, so after a restart it writes only if the hold started or ended while it was down.
- **Arbitration** (`internal/control`) decides an arbitrated capability. Rules, HomeKit and the device itself make claims at a level (Safety, Override, Normal). The highest level wins, the Device's `resolve` breaks ties within a level, and then the equipment limits hold the winner back. hap announces each Home app write on `oikade/v1/_homekit/<namespace>/<bridge>`, so the runtime can make it a HomeKit claim. hap also writes the device itself, so HomeKit works without the runtime.
- **The day planner** (`internal/schedule`) lays out a day in 15-minute slots that rules require, forbid or prefer, and spends a daily budget across them. Budgets on Rules are being built in [OIK-5](https://linear.app/oikade/issue/OIK-5), which also holds its design.

### Runtime state

The runtime keeps the state that must outlive a restart in retained MQTT messages, one per object (`engine.Retained`):

| Topic | Holds |
|---|---|
| `oikade/v1/_runtime/rules/<namespace>/<rule>` | A hold's active flag, latch and off-delay deadline |
| `oikade/v1/_runtime/control/<namespace>/<device>` | Claims and their expiries, released claims, and equipment-limit counters |

Namespaces never start with `_`, so these can't collide with a plugin's root. Losing them is safe but noticeable: the README's "State that must survive" says what's lost.

## HomeKit

hap (`internal/hap`) maps capabilities to HomeKit services and characteristics. Two kinds of number must never move, or users' Home app rooms, scenes and automations break:

- **Accessory IDs** are allocated by the Bridge controller and are append-only in `Bridge.status.accessories`: never reused or reordered, and kept for deleted Devices.
- **Service and characteristic IDs are positional.** `hap.NewServer` numbers them in the order they're added, so the mapping must only ever add at the end.

The bridge's identity is in `internal/identity`. It and the pairings are state that must survive (see the README).

brutella/hap serves HAP itself, except for one route. Its `/pairings` handler can't decode List Pairings, which the Home app sends before it adds a home hub, so `internal/hap/pairings.go` answers List Pairings from the pairing store and passes Add and Remove Pairing to the library unchanged ([OIK-75](https://linear.app/oikade/issue/OIK-75)). Unlike the HAP specification, it lets any verified controller list pairings, not only admins, because the library doesn't say which controller a session belongs to.

## Releases

release-please keeps a release PR open on main. Merging it tags the release, and `.github/workflows/release.yml` builds the arm64 and amd64 images and attaches `install.yaml` to the GitHub Release. Versions are `0.x` pre-releases (`0.1.0-alpha.N`) between milestones. Design: [OIK-63](https://linear.app/oikade/issue/OIK-63).
