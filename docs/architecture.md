# Architecture

How Oikade works inside, as it is now. It's for anyone changing the code. What a user writes and sees is in the [README](../README.md), and the plugin protocol is in [plugin-contract.md](plugin-contract.md).

This page says how things work, not why they were chosen. Each section links the Linear issue that holds the design and its discussion. When a change makes a section wrong, the same PR fixes it.

## Planes

| Plane | Owner | Holds |
|---|---|---|
| Control | Git, as CRDs (`home.oikade.dev`, `matter.oikade.dev`, `zigbee.oikade.dev`, `plugins.oikade.dev`) | What should exist: Devices, Rooms, Rules, Bridges, Zigbee2MQTT instances and Plugins |
| Data | MQTT (Mosquitto) | Live state and commands |
| History | Prometheus, Grafana and Loki | Values over time, changes and rule runs |

Live state never goes into etcd. `Device.status` is deliberately coarse: capabilities, conditions, claims at their transitions, and `lastSeen` at most every 5 minutes. Git owns Oikade's names (a Device's `metadata.name` and `displayName`), and apps own theirs ([OIK-130](https://linear.app/oikade/issue/OIK-130)). Drivers find a device by its ID, never by its name in the app. The operator renders each driver's configuration from Git and overwrites it on every start, but writes no names into it: Zigbee2MQTT's `configuration.yaml` points at `devices.yaml` on its PVC, which the init container seeds with the declared Devices' names only when it's missing. Something that exists but isn't declared, such as a paired but undeclared device, is drift: a metric and a log line. The runtime also lists every device each driver instance sees in its app in that instance's `status.discovered`, which only changes when a device is paired, removed or renamed. An instance with `spec.discovery.adopt` lets the operator adopt from it ([OIK-123](https://linear.app/oikade/issue/OIK-123)): `internal/controller/home/adopt_controller.go` creates a Device per undeclared device, with the instance as its controller owner, writes its spec once (the app's name included, which it never reads again), and lets go of it once any other field manager writes its spec. It also applies the instance's removal policy (`adopt_removal.go`): under `Delete`, an adopted Device whose device is missing from a reported list gets `RemovalPending` in its status, and is deleted once that condition's `lastTransitionTime` is older than the grace period. A list the runtime hasn't reported since it started is never read as empty. `adopt_replace.go` handles `oikade.dev/replace-with`: it moves the named adopted Device's ID onto the annotated one, then deletes it. Matter endpoints are keyed by Device name, so the endpoint stays.

## Components

One binary, `cmd/`, has four roles:

| Role | Talks to | Runs |
|---|---|---|
| `manager` | Kubernetes only | The controllers in `internal/controller/<group>`, which validate objects and render what they manage with `internal/render` |
| `manager runtime` | MQTT, and the API for status | One replica. The drivers, the hub, the status writers (Device status, and `status.discovered` on Plugins and Zigbee2MQTT instances), `/metrics` on `:8080`, and the rule engine with arbitration |
| `manager matter --bridge <name>` | MQTT, and the API read-only | One per Bridge, with `hostNetwork`. A Matter bridge node for that Bridge's Devices |

The runtime is a single replica because two would run every rule twice. Anything that assumes it's highly available, or needs leader election, is a design decision. The matter role follows the bus itself, so controllers keep working while the runtime is down. It can read no Secrets: its own are mounted.

### The data path

```
driver ──▶ hub ──▶ status writer, metrics exporter, rule engine   (runtime)
  ▲         └────▶ Matter bridge                                  (matter)
  └── Set ◀── engine (through arbitration), Matter bridge
```

- **A driver** translates one integration to capabilities. It reports to a `driver.Sink` (`Discovered`, `Changed`, `Availability`, `Seen`, `Undeclared`, and `Inventory`: every device it sees in its app, declared or not) and takes writes as a `driver.Setter`. The drivers are `internal/driver/zigbee2mqtt` and `internal/driver/plugin`. A new kind of integration is a new driver and a new member of the Device spec's driver union, and usually its own API group.
- **The hub** (`internal/hub`) holds the latest state of each device and fans updates out. Both the runtime and the matter role build one.
- **Capabilities** (`internal/capability`) are the protocol-neutral names and value types every consumer uses. Drivers convert to them, and nothing after the driver knows about vendor payloads.

### Drivers

- **Zigbee2MQTT.** The operator runs each instance and renders its `configuration.yaml` from Devices. The driver maps each device's exposes to capabilities.
- **Plugins** speak the [oikade/v1 contract](plugin-contract.md) under `oikade/v1/<namespace>/<plugin>/`. The operator runs each Plugin as a Deployment and gives it its Devices in `devices.json`. The driver validates every message against the contract and the capability registry, and drops and counts what doesn't fit. The Go SDK is `sdk/plugin`, and the contract types are `contract/v1`. Each plugin lives in its own repo and checks itself with the `oikade-conformance` command (`cmd/oikade-conformance`, wrapping `internal/conformance`), as [plugin-contract.md](plugin-contract.md#conformance) describes. Here, `make test-conformance` drives the simulated-relay fixture `test/fixtures/simswitch` through it, so an SDK change is tested end to end in its own PR. Design: [OIK-61](https://linear.app/oikade/issue/OIK-61) (plugin drivers) and [OIK-62](https://linear.app/oikade/issue/OIK-62) (the Go SDK).

## The host

The host is how Oikade will run ([OIK-143](https://linear.app/oikade/issue/OIK-143)), and **Kubernetes is still how it runs today**: nothing above changes yet. `oikade host` (`cmd/oikade`, `internal/host`) has simulated devices only, with no drivers, Matter or MQTT.

```
                 host (long-lived)                         your process (replaceable)
┌──────────────────────────────────────────┐          ┌───────────────────────────────┐
│ device state (hub.Store)                 │─changes─▶│ mirror of device state        │
│ arbitration (an engine with no rules)    │◀─claims──│ triggers, conditions, holds   │
│ device writes                            │◀─writes──│ (an engine with no arbiters)  │
│ the store: hold state, latches, claims,  │◀─saves───│                               │
│ the journal                              │─ping────▶│                               │
│ supervision, your binaries               │◀─pong────│                               │
└──────────────────────────────────────────┘          └───────────────────────────────┘
```

- **The split.** Both sides run the real `internal/engine`. The host's has no rules, so it only runs arbiters and their timers, and owns the store. Yours has every rule and no arbiters: `engine.Arbitration` sends its claims to the host, `engine.States` saves hold state there, and writes go through `driver.Setter`.
- **Your process** is your binary, built from `framework`, a public package. A home's `main` declares holds, moments and flows and calls `framework.Run`. Its exported API can't mention `internal/`: `internal/apicheck` fails a test if it does.
- **The mirror.** Conditions read a copy of the device state held in your process, so they never wait for a socket. So do hold states, read once from the `welcome`: the engine reads them with its lock held. Everything that changes the host's state is a request that waits for its answer, and none is sent with the lock held.
- **Surviving a kill.** Device state, claims and latches are in the host, so killing your process loses none of them. The next process gets the saved hold states and works each hold out again, and writes only if the hold started or ended meanwhile. A write and the save after it aren't atomic: a process killed between the two repeats the write, unless the capability is arbitrated, where a repeat claim changes nothing.
- **Supervision.** The host starts your process, restarts it with backoff, and kills one that stops answering. A condition stuck in a loop stops the answers. Exactly one process runs at a time, and the host refuses any other. [host-protocol.md](host-protocol.md) has the details, and says what happens to a process's claims.
- **Claims outlive a crash of the host.** Saving arbiters' state is a background job, which suits the runtime. The host's answer to a claim or a release is a promise, so it saves first (`Engine.flushControls`). An arbiter's other changes, such as a write it has made, still save in the background.
- **Durable flows** (`internal/flow`). A flow is ordinary Go, and [writing-flows.md](writing-flows.md) is how to write one. Each step sends a journal entry, the host writes it to the store, and only then does the flow continue. A step's effect comes first and its entry second, so a crash between the two repeats the effect: actions run at least once. After a crash the `welcome` returns every unfinished run, and the flow reruns from the top with recorded steps returning their recorded results. The first step that isn't recorded is where it carries on.
- **What can't resume.** An entry records a step's kind and arguments, so a replay notices code that takes different steps, and drops the run and logs it. A run past its latest useful time gives up, and logs that, unless it's a Safety flow. A run's ID is never reused, since the journal numbers a run from 1. Only the newest run of a flow resumes, and `internal/flow` stops a run by panicking through the flow, because a step has no error to return for these.
- **Times of day.** `Compiled.Since` comes from the host, which saves when each rule's timers were last accounted for. So a time missed while no process ran fires when one starts, within the same limit as a late flow, and a save lost to a crash means catching up once more.
- **Deploys.** The host keeps your binaries in the data directory, and the store says which is running. `oikade deploy` checks your flows and builds your module for the host's platform, and the host checks the result before anything stops. It stops the old process, starts the new one and watches it settle, and a failure brings the last one back. [host-protocol.md](host-protocol.md#deploys) has the order. `--home` only says what to run until the first deploy.
- **Fault points.** `internal/fault` marks where a crash matters, and compiles to nothing unless built with the `faultpoints` tag, which only `test/host` uses. `TestKilledAtEveryJournalBoundary` uses them to kill each side at every journal write, just before and just after it's durable. `TestBrokenWriteAheadFailsTheCheck` proves that test can fail.
- **Not yet.** Arbitrated devices are declared with CRD types, in the simulator's file.

## Rules

The engine (`internal/engine`) loads each Rule the manager has accepted, compiled with its CEL conditions. The manager checks Rules (`internal/controller/home`) and sets `Ready=False` on one it can't run, but only the runtime runs them. Design: [OIK-64](https://linear.app/oikade/issue/OIK-64) (the rule model) and [OIK-60](https://linear.app/oikade/issue/OIK-60) (room targets).

- **Moments** (`triggers`) run `do` when a trigger fires and the condition holds. Time and sun triggers are timers, with sun times from `internal/sun`. A Room target resolves to its members when the trigger or action runs, not when the Rule loads, so a Device added to a room is covered at once.
- **Holds** (`while`) aren't timed in memory. The engine works out from the clock and the devices' state whether each hold is active, and when that next changes. It saves each hold's state, so after a restart it writes only if the hold started or ended while it was down.
- **Arbitration** (`internal/control`) decides an arbitrated capability. Rules, Matter controllers and the device itself make claims at a level (Safety, Override, Normal). The highest level wins, the Device's `resolve` breaks ties within a level, and then the equipment limits hold the winner back. The Matter bridge announces each controller's write on `oikade/v1/_matter/<namespace>/<bridge>/writes`, so the runtime can make it a `Matter` claim. It also writes the device itself, so controllers work without the runtime.
- **Rules in Go** (`internal/engine/rule.go`). A Rule's CEL becomes a Go `Condition`, and `engine.Compile` turns a CRD into the engine's own input. `NewMoment`, `NewHold` and `NewTrigger` build the same input from Go, with no CRD, for [the host](#the-host). Budgets and a Device's `control` entries still use CRD types. A condition runs with the engine's lock held, so it must not block.
- **The day planner** (`internal/schedule`) lays out a day in 15-minute slots that rules require, forbid or prefer, and spends a daily budget across them. Budgets on Rules are being built in [OIK-5](https://linear.app/oikade/issue/OIK-5), which also holds its design.

### Runtime state

The runtime keeps the state that must outlive a restart in retained MQTT messages, one per object (`engine.Retained`):

| Topic | Holds |
|---|---|
| `oikade/v1/_runtime/rules/<namespace>/<rule>` | A hold's active flag, latch and off-delay deadline |
| `oikade/v1/_runtime/control/<namespace>/<device>` | Claims and their expiries, released claims, and equipment-limit counters |

Namespaces never start with `_`, so these can't collide with a plugin's root. Losing them is safe but noticeable: the README's "State that must survive" says what's lost.

### Embedded store

`internal/store` is [the host's](#the-host) state store. The runtime still keeps its state on the broker, as above.

- One data directory holds one [bbolt](https://github.com/etcd-io/bbolt) file, `oikade.db`, and only one process can have it open. The operating system drops that lock when the process dies, so a crash never leaves it locked. A second process gets a clear error.
- Namespaces share the file with separate keys, for the registry, latches, claims and Matter state to come. `engine.Stored` implements `engine.States` on a namespace, as `Retained` does on the broker.
- The journal keeps ordered entries per run. `Append` returns only once its entry is on disk. Each write is one transaction with two fsyncs, so its cost is about twice the disk's fsync time. A crash mid-write loses only the entry that was never acknowledged.
- The file records its format version, and `Open` refuses one it doesn't know.

## Matter

Oikade reaches the Home app and other controllers as a Matter bridge. It replaced a HomeKit bridge built on an unmaintained library ([OIK-76](https://linear.app/oikade/issue/OIK-76)). The Matter stack is Oikade's own, in `internal/matter`, built from the Matter Core Specification: TLV, the message layer with MRP over UDP, PASE and CASE, operational certificates, the Interaction Model, the commissioning clusters, attestation with connectedhomeip's test DAC, and an mDNS responder. `internal/matter/node` wires them into a node serving a bridge, and `manager matter` runs one.

- **The matter Bridge controller** (`internal/controller/matter`) creates the `<bridge>-matter` Secret once, with a random setup passcode and discriminator, a unique ID and the test DAC and key. It keeps the pairing codes derived from them in the Secret too (`manual-code`, `qr-code`), and only the discriminator in status. It allocates endpoint IDs densely from 2 in `status.endpoints`, append-only, and keeps a copy in the Secret, written before status, so a Bridge applied again gets them back. It allocates the UDP port, and renders the `<bridge>-matter` PVC and Deployment.
- **The pod** reads the setup code and attestation from the Secret, mounted as files, and keeps its fabric table on the PVC. It reads Bridges and Devices, and its Role grants no Secrets and no writes. It builds its own hub and follows the bus.
- **The mapping** (`internal/matterbridge`) makes each selected Device with an endpoint ID one bridged endpoint (one annotated `oikade.dev/matter: hide` isn't selected, and gets no endpoint until it's shown), with a device type per function (on/off or dimmable light, plug-in unit, mounted control, occupancy, contact, temperature, humidity, and Power Source for a battery) and their clusters. It builds endpoints from Device status, not the live store, so they're there from the start. A Device whose capabilities or device type change is rebuilt on the same endpoint ID. A new `displayName` isn't a rebuild: it sets the Bridged Device Basic Information's NodeLabel in place, and subscribers are sent it ([OIK-130](https://linear.app/oikade/issue/OIK-130)). Bus state reaches attributes under the node's lock, and availability is the Bridged Device Basic Information's Reachable.
- **Writes** from controllers go through a coalescer: per device and capability the last command in a 50 ms window wins, and one that matches the device's state is dropped. Then they're announced, for arbitrated capabilities, and written through the hub.
- **Fabrics** reach status without the pod writing to the API: it publishes them retained on `oikade/v1/_matter/<namespace>/<bridge>/fabrics`, and the runtime patches `Bridge.status.fabrics`. The controller sets `DevicesSupported` from Device status, with the mapping's `Supported`.
- **Commissioning** (`internal/matter/clusters`) owns every commissioning window, including the initial one the node opens while it's on no fabric, which closes after 15 minutes, on RevokeCommissioning, or once commissioned. The node advertises each fabric's operational service when AddNOC or UpdateNOC changes it, and again when the fail-safe rolls it back. Ending a session sends CloseSession, and a peer's CloseSession ends its session.

## Releases

release-please keeps a release PR open on main. Merging it tags the release, and `.github/workflows/release.yml` builds the arm64 and amd64 images and attaches `install.yaml` to the GitHub Release. A milestone is a full release (`0.1.0`), and the versions between milestones are pre-releases of the next one (`0.2.0-alpha.N`). Design: [OIK-63](https://linear.app/oikade/issue/OIK-63).
