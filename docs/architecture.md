# Architecture

How Oikade works inside, as it is now. It's for anyone changing the code. What a user writes and sees is in the [README](../README.md), and the plugin protocol is in [plugin-contract.md](plugin-contract.md).

This page says how things work, not why they were chosen. Each section links the Linear issue that holds the design and its discussion. When a change makes a section wrong, the same PR fixes it.

## Planes

| Plane | Owner | Holds |
|---|---|---|
| Control | Git, as CRDs (`home.oikade.dev`, `matter.oikade.dev`, `zigbee.oikade.dev`, `plugins.oikade.dev`) | What should exist: Devices, Rooms, Rules, Bridges, Zigbee2MQTT instances and Plugins |
| Data | MQTT (Mosquitto) | Live state and commands |
| History | Prometheus, Grafana and Loki | Values over time, changes and rule runs |

Live state never goes into etcd. `Device.status` is deliberately coarse: capabilities, conditions, claims at their transitions, and `lastSeen` at most every 5 minutes. Git owns names: the operator renders each driver's configuration from Devices and overwrites it on every start. Something that exists but isn't declared, such as a paired but undeclared device, is drift: a metric and a log line, never adopted.

## Components

One binary, `cmd/`, has four roles:

| Role | Talks to | Runs |
|---|---|---|
| `manager` | Kubernetes only | The controllers in `internal/controller/<group>`, which validate objects and render what they manage with `internal/render` |
| `manager runtime` | MQTT, and the API for status | One replica. The drivers, the hub, the status writer, `/metrics` on `:8080`, and the rule engine with arbitration |
| `manager matter --bridge <name>` | MQTT, and the API read-only | One per Bridge, with `hostNetwork`. A Matter bridge node for that Bridge's Devices |

The runtime is a single replica because two would run every rule twice. Anything that assumes it's highly available, or needs leader election, is a design decision. The matter role follows the bus itself, so controllers keep working while the runtime is down. It can read no Secrets: its own are mounted.

### The data path

```
driver ──▶ hub ──▶ status writer, metrics exporter, rule engine   (runtime)
  ▲         └────▶ Matter bridge                                  (matter)
  └── Set ◀── engine (through arbitration), Matter bridge
```

- **A driver** translates one integration to capabilities. It reports to a `driver.Sink` (`Discovered`, `Changed`, `Availability`, `Seen`, `Undeclared`) and takes writes as a `driver.Setter`. The drivers are `internal/driver/zigbee2mqtt` and `internal/driver/plugin`. A new kind of integration is a new driver and a new member of the Device spec's driver union, and usually its own API group.
- **The hub** (`internal/hub`) holds the latest state of each device and fans updates out. Both the runtime and the matter role build one.
- **Capabilities** (`internal/capability`) are the protocol-neutral names and value types every consumer uses. Drivers convert to them, and nothing after the driver knows about vendor payloads.

### Drivers

- **Zigbee2MQTT.** The operator runs each instance and renders its `configuration.yaml` from Devices. The driver maps each device's exposes to capabilities.
- **Plugins** speak the [oikade/v1 contract](plugin-contract.md) under `oikade/v1/<namespace>/<plugin>/`. The operator runs each Plugin as a Deployment and gives it its Devices in `devices.json`. The driver validates every message against the contract and the capability registry, and drops and counts what doesn't fit. The Go SDK is `sdk/plugin`, and the contract types are `contract/v1`. Each plugin lives in its own repo and checks itself with the `oikade-conformance` command (`cmd/oikade-conformance`, wrapping `internal/conformance`), as [plugin-contract.md](plugin-contract.md#conformance) describes. Here, `make test-conformance` drives the simulated-relay fixture `test/fixtures/simswitch` through it, so an SDK change is tested end to end in its own PR. Design: [OIK-61](https://linear.app/oikade/issue/OIK-61) (plugin drivers) and [OIK-62](https://linear.app/oikade/issue/OIK-62) (the Go SDK).

## Rules

The engine (`internal/engine`) loads each Rule the manager has accepted, compiled with its CEL conditions. The manager checks Rules (`internal/controller/home`) and sets `Ready=False` on one it can't run, but only the runtime runs them. Design: [OIK-64](https://linear.app/oikade/issue/OIK-64) (the rule model) and [OIK-60](https://linear.app/oikade/issue/OIK-60) (room targets).

- **Moments** (`triggers`) run `do` when a trigger fires and the condition holds. Time and sun triggers are timers, with sun times from `internal/sun`. A Room target resolves to its members when the trigger or action runs, not when the Rule loads, so a Device added to a room is covered at once.
- **Holds** (`while`) aren't timed in memory. The engine works out from the clock and the devices' state whether each hold is active, and when that next changes. It saves each hold's state, so after a restart it writes only if the hold started or ended while it was down.
- **Arbitration** (`internal/control`) decides an arbitrated capability. Rules, Matter controllers and the device itself make claims at a level (Safety, Override, Normal). The highest level wins, the Device's `resolve` breaks ties within a level, and then the equipment limits hold the winner back. The Matter bridge announces each controller's write on `oikade/v1/_matter/<namespace>/<bridge>/writes`, so the runtime can make it a `Matter` claim. It also writes the device itself, so controllers work without the runtime.
- **The day planner** (`internal/schedule`) lays out a day in 15-minute slots that rules require, forbid or prefer, and spends a daily budget across them. Budgets on Rules are being built in [OIK-5](https://linear.app/oikade/issue/OIK-5), which also holds its design.

### Runtime state

The runtime keeps the state that must outlive a restart in retained MQTT messages, one per object (`engine.Retained`):

| Topic | Holds |
|---|---|
| `oikade/v1/_runtime/rules/<namespace>/<rule>` | A hold's active flag, latch and off-delay deadline |
| `oikade/v1/_runtime/control/<namespace>/<device>` | Claims and their expiries, released claims, and equipment-limit counters |

Namespaces never start with `_`, so these can't collide with a plugin's root. Losing them is safe but noticeable: the README's "State that must survive" says what's lost.

## Matter

Oikade reaches the Home app and other controllers as a Matter bridge. It replaced a HomeKit bridge built on an unmaintained library ([OIK-76](https://linear.app/oikade/issue/OIK-76)). The Matter stack is Oikade's own, in `internal/matter`, built from the Matter Core Specification: TLV, the message layer with MRP over UDP, PASE and CASE, operational certificates, the Interaction Model, the commissioning clusters, attestation with connectedhomeip's test DAC, and an mDNS responder. `internal/matter/node` wires them into a node serving a bridge, and `manager matter` runs one.

- **The matter Bridge controller** (`internal/controller/matter`) creates the `<bridge>-matter` Secret once, with a random setup passcode and discriminator, a unique ID and the test DAC and key. It keeps the pairing codes derived from them in the Secret too (`manual-code`, `qr-code`), and only the discriminator in status. It allocates endpoint IDs densely from 2 in `status.endpoints`, append-only, and keeps a copy in the Secret, written before status, so a Bridge applied again gets them back. It allocates the UDP port, and renders the `<bridge>-matter` PVC and Deployment.
- **The pod** reads the setup code and attestation from the Secret, mounted as files, and keeps its fabric table on the PVC. It reads Bridges and Devices, and its Role grants no Secrets and no writes. It builds its own hub and follows the bus.
- **The mapping** (`internal/matterbridge`) makes each selected Device with an endpoint ID one bridged endpoint, with a device type per function (on/off or dimmable light, plug-in unit, mounted control, occupancy, contact, temperature, humidity, and Power Source for a battery) and their clusters. It builds endpoints from Device status, not the live store, so they're there from the start. A Device whose capabilities change is rebuilt on the same endpoint ID. Bus state reaches attributes under the node's lock, and availability is the Bridged Device Basic Information's Reachable.
- **Writes** from controllers go through a coalescer: per device and capability the last command in a 50 ms window wins, and one that matches the device's state is dropped. Then they're announced, for arbitrated capabilities, and written through the hub.
- **Fabrics** reach status without the pod writing to the API: it publishes them retained on `oikade/v1/_matter/<namespace>/<bridge>/fabrics`, and the runtime patches `Bridge.status.fabrics`. The controller sets `DevicesSupported` from Device status, with the mapping's `Supported`.
- **Commissioning** (`internal/matter/clusters`) owns every commissioning window, including the initial one the node opens while it's on no fabric, which closes after 15 minutes, on RevokeCommissioning, or once commissioned. The node advertises each fabric's operational service when AddNOC or UpdateNOC changes it, and again when the fail-safe rolls it back. Ending a session sends CloseSession, and a peer's CloseSession ends its session.

## Releases

release-please keeps a release PR open on main. Merging it tags the release, and `.github/workflows/release.yml` builds the arm64 and amd64 images and attaches `install.yaml` to the GitHub Release. A milestone is a full release (`0.1.0`), and the versions between milestones are pre-releases of the next one (`0.2.0-alpha.N`). Design: [OIK-63](https://linear.app/oikade/issue/OIK-63).
