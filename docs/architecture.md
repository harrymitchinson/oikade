# Architecture

How Oikade works inside: what each part owns, the rules it must keep, and where it lives in the code. It's for anyone changing Oikade. What a user writes and sees is in the [README](../README.md) and the guides it lists. The reference pages are the [plugin contract](plugin-contract.md), the [host protocol](host-protocol.md) between the host and your process, and the [host API](host-api.md) that `oikade` calls.

This page says how things work, not why they were chosen. A section names the plan issue that holds its design and discussion once, under its heading (`docs/planning.md`). When a change makes a section wrong, the same PR fixes it.

## Who owns what

| Owner | Holds |
|---|---|
| Git, as a Go module | Your automations: decisions, flows, arbitration settings, and a copy of the device list (`oikade types`) |
| The host's data directory | Devices, names, rooms and endpoints (the registry), claims, latches, flows' journals, deployed versions and their values, Matter's identity and fabrics, and what plugins' setups kept (secrets) |
| Apps | Their own names. Drivers find a device by its ID, never by its name in the app, and the host copies a name once (OIK-130) |
| Prometheus, Grafana and Loki | Values over time, changes and rule runs |

## Two binaries

Design: OIK-170.

- **`oikade-host`** (`cmd/oikade-host`, `internal/host`) is the host: one long-lived process per home, with your process and each plugin as children it supervises.
- **`oikade`** (`cmd/oikade`) is every command you run, from a laptop or CI. It reaches the host over [its API](#the-api), and links neither `internal/host` nor Matter, which `test/cli` checks.
- **`internal/cli`** is what both share as commands: reading flags, the look of their output, and the version.

There's no second host, because two would run every rule twice. Anything that assumes high availability, or needs leader election, is a design decision.

## The data path

```
driver ──▶ hub ──▶ metrics, event log, your process, Matter bridge
  ▲
  └── Set ◀── arbitration ◀── your process, Matter bridge, oikade set
```

- **A driver** translates one integration to capabilities. It reports to a `driver.Sink` (`Discovered`, `Changed`, `Availability`, `Seen`, `Undeclared`, and `Inventory`: every device it sees in its app, declared or not), and takes writes as a `driver.Setter`. The host has two: `internal/driver/plugin`, which decodes every [plugin](#plugins)'s messages, and the simulator (`internal/host/simulator.go`, for `--simulate`).
- **The hub** (`internal/hub`, `hub.Store`) holds the latest state of each device and fans updates out.
- **Capabilities** (`internal/capability`) are the protocol-neutral names and value types every consumer uses. Drivers convert to them, so nothing after a driver knows a vendor's payloads. [capabilities.md](capabilities.md) lists them.

A `driver.Sink` is called on the driver's receive goroutine, and must not block.

## The host

Design: OIK-143.

```
                 host (long-lived)                         your process (replaceable)
┌──────────────────────────────────────────┐          ┌───────────────────────────────┐
│ device state (hub.Store)                 │─changes─▶│ mirror of device state        │
│ arbitration (an engine with no rules)    │◀─claims──│ triggers, conditions, holds   │
│ device writes                            │◀─writes──│ (an engine with no arbiters)  │
│ the store: hold state, latches, claims,  │◀─saves───│                               │
│ the journal                              │─ping────▶│                               │
│ supervision, your binaries               │◀─pong────│                               │
│ the Matter bridge                        │          │                               │
└──────────────────────────────────────────┘          └───────────────────────────────┘
```

`oikade-host install` (`internal/install`) puts the host under systemd, or upgrades it, on the machine itself. Its steps take a root directory, a command runner and the socket as a function, so its tests run with a fake `systemctl` (OIK-277).

### The split with your process

Both sides run the real `internal/engine`. The host's engine has no rules: it runs the arbiters and their timers, and owns the store. Yours has every rule and no arbiters. `engine.Arbitration` sends its claims to the host, `engine.States` saves hold state there, and writes go through `driver.Setter`.

Your process is your binary, built from `oikade`, the public package at the module root. A home's `main` adds decisions, flows and plugins to an `oikade.Home` and calls its `Run`. Its exported API can't mention `internal/`, which `internal/apicheck` checks in a test.

A write's context says whose it is (`driver.WithWriter`). The engine marks a rule's, the socket carries it as the set's `source`, and the host's `recordedSetter`, which every write to a device goes through, puts it in the event log as the write's `by`. A controller's write and `oikade set`'s are marked the same way.

[host-protocol.md](host-protocol.md) has every message between the two.

### Locks, and what must not block

- **Conditions read a mirror.** Your process keeps a copy of the device state, so a condition never waits for a socket. Hold states are read once, from the `welcome`. The engine reads both with its lock held.
- **A condition runs with the engine's lock held, so it must not block.** Your process answers the host's heartbeat only once that lock is free, so a condition stuck in a loop stops the answers, and the host replaces the process.
- **Nothing that changes the host's state is sent with the lock held.** Each such change is a request that waits for its answer.
- **Nothing that reports an event waits.** The event log's `add` only appends to a queue, and `internal/host/feed.go` fans events out to tails without ever waiting for a reader.

### Supervision

The host starts your process, restarts it with backoff, and kills one that stops answering. Exactly one automations process runs at a time, and the host refuses any other. [host-protocol.md](host-protocol.md#heartbeats) has the heartbeat's timings, [when a session ends](host-protocol.md#when-a-session-ends) the restart backoff, and [what happens to claims](host-protocol.md#claims-when-a-process-goes-away) when a process goes.

### Times of day

Sunrise and sunset are worked out in your process by `internal/sun`, for the location in the home's `oikade.Options`, which `oikade` gives `Engine.Site`. `internal/simhome` takes the same `Site`, so `oikade dev`, `oikade replay` and `oikadetest` agree with the house. The host's engine only arbitrates, so it has none.

`Compiled.Since` comes from the host, which saves when each rule's timers were last accounted for (`saveFire`). So a time missed while no process ran fires when one starts, within the same limit as a late flow. A save lost to a crash means catching up once more.

### Pauses

The store holds them. The host tells your process in the `welcome`, then with `pause` and `resume` messages, and the process keeps a paused rule out of its engine. The host ends the rule's claims below Safety itself, since no process may be running to do it.

### Metrics

Host and device metrics come from the host. Rules run in your process, which sends its metrics in Prometheus's text format every 5 seconds, so Prometheus scrapes one target that outlives deploys (`internal/host/observe.go`). The HTTP listener only reads. [metrics.md](metrics.md) lists them.

The Matter bridge's come from an `im.Observer` the node hands its Interaction Model server, and from its fabrics, sessions and subscriptions, read at each scrape (`internal/host/matter_metrics.go`). Their labels never carry a node ID, which a controller chooses.

### Stopping

`Host.Run` keeps each part in a `stage` with its own context (`internal/host/shutdown.go`), so a part ends when its phase comes, not when the signal does. The phases run in order:

1. **The API.**
2. **Your automations.**
3. **Writes under way.** A write is under way until the driver has it (`routedSetter`), while the engine has work in flight (`Engine.Idle`), and, for a controller's, until its burst has been sent (`Coalescer.Busy`). This wait has a limit of its own, so a stuck write can't take the plugins' turn.
4. **Plugins.** The contract has no acknowledgement of a `/set`, so what a plugin does with one it has been given is up to it as it handles `SIGTERM`. A plugin's socket ends by itself once its process has gone, so everything it said before going is read.
5. **Matter.** The node sends CloseSession on each session, within a second, before its socket and mDNS responder close, so a controller learns at once rather than at its own timeout (OIK-217).
6. **The engine and the store.** The engine goes late because a controller's write goes through it.

`--stop-timeout` bounds the whole order. A phase still running at the deadline is left behind, the processes still running are killed, and the host exits with an error without closing the store, as closing would wait for it. That's safe because every write is on disk before anything depends on it. Only events still queued for the event log are lost.

## Crash safety

The host only ever crashes safe: a write reaches disk before anything depends on it. This section is the order each write goes in, and why a kill between two steps is safe.

### What's durable

| On disk before anything depends on it | Not durable |
|---|---|
| Every request from your process: a claim, a release, a hold's state, a timer's last fire, an average, a journal entry | The event log, which commits every 5 seconds ([the event log](#the-event-log)) |
| The registry, the home's settings, declarations and options, and pauses | An arbiter's other changes, such as a write it has made, which save in the background |
| Matter's identity and fabrics | Your process's state, which it rebuilds from the host's |
| A deploy's upload, its values and the version history, and a setup's secrets | Setup sessions, login codes, and what a session told the host for `oikade status` |

### Holds

Device state, claims and latches are in the host, so killing your process loses none of them. The next process gets the saved hold states, works each hold out again, and writes only if the hold started or ended meanwhile.

A write and its save aren't atomic, so each goes in the order a kill between them can repair (`Engine.saveStart`, OIK-299):

| What | Order | A kill in between |
|---|---|---|
| A start that only claims, as when every device it reaches is arbitrated | Save as active, then claim | The next process loads it active and claims again, latch and all. A claim outlives your process, so saving after would leave it with nothing to end it |
| A start that writes | Write, then save | A plain write ends with your process, so the next one writes the start again if it's still due |
| An end | Release or write, then save | The next process ends the hold again |

### Claims

Saving arbiters' state is a background job, but the host's answer to a claim or a release is a promise, so it saves first (`Engine.flushControls`). So claims outlive a crash of the host.

### Durable flows

`internal/flow` runs flows, which are ordinary Go. [writing-flows.md](writing-flows.md) is how to write one.

- **Actions run at least once.** Each step sends a journal entry, the host writes it to the store, and only then does the flow continue. A step's effect comes first and its entry second, so a crash between them repeats the effect.
- **A run resumes by replaying.** After a crash the `welcome` returns every unfinished run, and the flow runs again from the top, with recorded steps returning their recorded results. The first step that isn't recorded is where it carries on.
- **Code that takes different steps is caught.** An entry records a step's kind and arguments, so a replay notices, and drops the run and logs it. `internal/flowcheck` rejects some of this before a deploy.
- **A late run gives up.** One that wakes more than its limit after it was due (its `Flow.Late`, or `DefaultLate`) logs that and stops, unless it's a Safety flow.
- **Only the newest run of a flow resumes.** A run's ID is never reused, since the journal numbers a run's entries from 1 again once it's deleted.

`internal/flow` stops a run by panicking through the flow, because a step has no error to return for these.

### Fault points

`internal/fault` marks where a crash matters. It compiles to nothing unless built with the `faultpoints` tag, which only `test/host` uses.

- `TestKilledAtEveryJournalBoundary` kills each side at every journal write, just before and just after it's durable. `TestBrokenWriteAheadFailsTheCheck` proves that test can fail.
- The registry, the home's options, the Matter fabric table, a setup's secret, a deploy's upload and its values each have a point between their write reaching disk and its answer. `test/host/faults_test.go` kills the host there and starts it again, to show that what it was told survives.
- A hold's start has one after its claim (`client.hold.started`), where a test holds your process, lets the hold's condition pass and kills it.
- A test can plan a fault once the host is running: a host reads the plan at its first hit after the file appears.

## Deploys

The host keeps your binaries in the data directory, and the store says which is running (`internal/host/deploy.go`, `versions.go`). `oikade deploy` checks your flows and builds your module for the host's platform, and the host runs the result's check mode before anything stops. It stops the old process, starts the new one and watches it settle, and a failure brings the last one back. `--home` only says what to run until the first deploy.

[host-protocol.md](host-protocol.md#deploys) has the order, and what a failed version leaves behind.

### Your values

Design: OIK-365. The code is `internal/host/vars.go`.

- **A binary and its values go together.** The store has no transactions across keys, so a deploy's values are in the same record as the version history (`history.Vars`), with the last ones (`VarsBefore`) while the version is unconfirmed. An abort puts those back, and a crash anywhere leaves a binary and values that go together.
- **The host has both before it starts.** A deploy's body is `multipart/form-data`, the values and then the binary, and only the closing boundary counts as the end.
- **A deploy can keep the host's.** A body with no values' part, from `oikade deploy --keep-vars`, runs the version with the host's set, as a rollback does (`incoming`). The CLI then neither reads `.env` nor runs check mode to learn the names, since only the host knows its set.
- **Check mode names the values the code uses.** It records each `oikade.Var` as the home is built, so the CLI refuses a missing one before it uploads, and the host refuses one too, after its own check.
- **Check mode runs with values,** so code that reads one as the home is built is checked as it will run. The host writes the set a version would run with to a file of its own for its check (`varsFile.forCheck`). `oikade check`, `dev`, `types` and `deploy` write `.env`'s to a temporary file, mode 0600 (`homeValues`), with `OIKADE_VARS_FROM_ENVIRONMENT=1`, which lets the environment win for any name. The host never sets it.
- **Processes read them lock-free** (`versions.varsNow`). Before each process the host starts from your binary, `varsFile` writes them whole by a rename, mode 0600, in the runtime directory (`/run/oikade`, in memory) or else the data directory, and names the file in `OIKADE_VARS_FILE`. A host removes any file of values one before it left, as it starts.
- **A values-only deploy** is a deploy of the running version through `takeOver`, with its settle and restore. A plugin's process starts again when its binary or the values' digest changes (`generation`).
- **No value is in a log, an answer or an error**, only names. The digest isn't logged either.

### Secrets

What plugins' setups keep, such as a device's key, is a directory in the data directory with one file each (`internal/host/secrets.go`).

- The host passes on the directory as `OIKADE_SECRETS_DIR`, never a value, so nothing reads one but the code it's for.
- A setup's `keep` is the one step that carries a value, and it's in no log or answer.
- No call lists, sets or removes them, and a deploy leaves them. What you'd type in, a password included, is one of [your values](#your-values) instead, which a deploy brings.
- `mqtt.Connect` doesn't quote its URL in errors, and the host's logs use `redacted`, since a URL can carry a password.

## The API

Design: OIK-171. The code is `internal/host/api`, and the routes are in [host-api.md](host-api.md).

- **Two sockets.** The API is HTTP on `api.sock`, one connection a call, with newline-delimited JSON for streams. It's apart from `host.sock`, which only your process and plugins use. Both are `0600`.
- **Two transports.** `api.Local` dials the socket, which needs no authentication of its own. `api.Remote` dials the network API, which `--api-addr` serves over TLS (`internal/host/apiauth.go`), with a self-signed key in the store's `api` namespace and a bearer token for each client, kept as its SHA-256. A client trusts the host by its key's pin.
- **A panic in a call crashes the host,** as it does anywhere else (`crashes`), rather than net/http recovering it and leaving what it held half done.

## The device registry

`internal/registry` is the host's list of devices, which `internal/host/devices.go` keeps the hub in step with. Each driver instance gets the registry's devices: the hub's key is the registry's name, and a driver finds that from its hardware's ID.

### Endpoints

- **The store holds the registry as one value,** so a device and the endpoint counter can't disagree after a crash. An unreadable one stops the host, rather than starting empty and handing out endpoints again.
- **An endpoint belongs to one entry for good.** `Next` only rises, forgotten devices stay on record, and a name moved onto new hardware keeps its entry's endpoint and retires the replacement's.
- **A device stays until you forget it.** Your process isn't told a device went: it's told about new and changed ones, and a deploy gives it a fresh list.

### Claims follow names

Claims are keyed by a device's name.

- **A declared rename moves them** (`Engine.MoveDevices`), before the hub renames, with the old name given no arbiters meanwhile. The device's own cases' claims, holds, fires, pauses and averages move to the new name's keys: only those the new version's `Control` has on the device (`wire.OwnCases`), since a group's cases can be keyed by a name a device has too. A failure puts everything back.
- **Forgetting a device ends its claims** (`Engine.ForgetDevice`), rather than leaving them for the next device of that name. Claims it has only on disk count too (`Engine.SavedClaims`).
- **A claims record that can't be read is a Safety claim on the whole device** (`Engine.UnreadableClaim`). It stays, no arbiter starts empty to save over it, `claimedSetter` refuses every write to the device, and removing the device with `--allow-safety` clears it.

### Arbitration settings

- **They're on record with the device,** so a host loads its arbiters as soon as a driver instance attaches, and a claim holds before the device reports.
- **A home's `Control` arrives in the process's `hello`,** and overlays them by capability. It's saved first, in its own namespace, so a host that restarts arbitrates what the last code said before any process attaches.
- **A process only adds and changes settings** (`inventory.takeHome`), and a process turned away has no say. Dropping one ends its claims, so that waits for the version to settle (`deployer.removals`), and a deploy that would end a Safety claim that way is refused without `--allow-safety`.
- **A deploy refuses declarations that take a device out of a Safety case's room write,** by its room or type (`inventory.leavingSafety`), comparing each case with the same one in the old version.

### `oikade types`

`oikade types` renders the registry to Go (`internal/registry/types.go`), as the home's declarations would make it (`registry.Declaring`). It reads the declarations from the manifest of the home it builds, or from its source when it doesn't build, as when it names a handle only the new file has.

The file has a value per device, with its name and an `oikade.Bool`, `oikade.Number` or `oikade.Choice` for each capability, named by the capability registry's `Go` name, so the compiler checks what a home names. Nothing else is in it: `oikade dev` and `oikadetest` read the handles back from its Go (`registry.Handles`), with rooms and types from the home's declarations.

## Plugins

Design: OIK-61 (plugin drivers) and OIK-62 (the Go SDK). The contract is [plugin-contract.md](plugin-contract.md).

### In the host

`internal/host/plugins.go` runs them.

- **One process each.** Each plugin your binary imports runs as `<binary> plugin <name>`, with the same supervision and heartbeat as your automations (`Host.watch`) and its own backoff. The host learns their names from check mode.
- **Only a new binary or new values restart one.** So a deploy does, and a crash of your automations doesn't.
- **One decoder for every plugin.** A plugin process speaks `internal/host/pluginwire`, the part of the protocol with no engine types, so the SDK doesn't link the engine. `socketBus` makes its session look like an `mqtt.Bus`, so `internal/driver/plugin` decodes imported and outside plugins alike. It validates every message against the contract and the capability registry, and drops and counts what doesn't fit.
- **A plugin outlives its processes.** Its decoder stays, and keeps a process's devices known while none runs. Every plugin on record attaches at start, so a claim on its devices is enforced before any process connects.
- **Writes are routed.** `routedSetter` sends each write to the device's driver, for your code, `oikade set` and Matter alike.
- **A plugin needs no list of devices.** After a `devices` report, the host records what it hears in the registry, and tells the decoder which device each ID is.
- **Claims only on disk count.** `Engine.RuleClaims` and `EndRule` see claims on a device whose driver is yet to attach, so a deploy's check can't miss one.

### The SDK and conformance

The Go SDK is `sdk/plugin`, and the contract's types are `contract/v1`. The SDK's `Plugin` publishes through a `link`, a broker or the host's socket, so a `Handler` is the same on both.

First-party plugins live in `plugins/`, and anyone else's in its own repo, which checks itself with `oikade-conformance` (`cmd/oikade-conformance`, wrapping `internal/conformance`), as [plugin-contract.md](plugin-contract.md#conformance) says. Here, `make test-conformance` drives the simulated-relay fixture `test/fixtures/simswitch` through the suite, so an SDK change is tested end to end in its own PR.

### First-party plugins

Each plugin's README (`plugins/<name>/README.md`) is its user docs, and its package doc says how it speaks its platform.

- **eWeLink LAN** (`plugins/ewelinklan`, OIK-10) speaks the eWeLink LAN protocol. It finds devices by following `_ewelink._tcp` over mDNS, through `internal/mdns`, and decrypts each report with the device's LAN key, which its setup fetches from your eWeLink account and keeps as a secret. A write is an encrypted POST to the device, and `Set` returns once a newer report shows the relay moved. A device with no key has that as its problem, and its availability follows the age of its last report.
- **Hue** (`plugins/hue`, OIK-384) speaks a Hue bridge's local API, CLIP v2. It finds the bridge by its ID over mDNS (`_hue._tcp`), through `internal/mdns`, and checks its certificate against Philips Hue's roots and that ID. It opens the bridge's event stream, then reads every resource, and applies each change from the stream over the read. `Set` is a paced PUT, and returns once the bridge has taken it: the stream then shows the change. A device is named by its Zigbee MAC, and online while the bridge reaches it over Zigbee. A stream that ends after a minute is opened again at once, and anything else makes every device offline and backs off.
- **Overkiz** (`plugins/overkiz`, OIK-17) speaks Overkiz's enduser API, to a TaHoma hub's local API or to Cozytouch's cloud, and for a Cozytouch account to Atlantic's Magellan cloud beside it (OIK-329). It reads the setup's devices, then follows an event listener; Magellan has none, so it's read every minute. `Set` returns once the execution is accepted. A refused credential is tried again hourly, or with the deploy that brings a new one, and anything else backs off. While there's no session every device is offline, and the plugin's status says why.
- **Toshiba Home AC Control** (`plugins/toshibaac`, OIK-256) speaks the app's cloud protocol, since the units' WiFi modules only connect out: an HTTPS login, then MQTT 3.1.1 to the Azure IoT Hub the login names. The hub calls a direct method with each unit's state, and every unit is read over HTTP every 30 minutes too. `Set` returns on the hub's PUBACK, and refuses a write that would clash with a unit sharing its outdoor unit (`Config.OutdoorUnits`, OIK-323). A refused login is tried again hourly, a rate limit waits as long as the cloud asks (OIK-361), and anything else backs off.
- **Zigbee2MQTT** (`plugins/zigbee2mqtt`, OIK-155) follows a Zigbee2MQTT and broker you run, and the host starts neither. It reports every paired device by IEEE address, maps each device's exposes to capabilities, and sends a paced `/get` for a readable value it has yet to hear (OIK-53). `Set` publishes the `/set`, and the result arrives as Zigbee2MQTT reports it. Zigbee2MQTT's `bridge/state`, its Last Will, and the plugin's own broker connection are its health: either going offline marks its devices offline and refuses writes.

## Matter

Design: OIK-76.

Oikade reaches the Home app and other controllers as a Matter bridge, which runs in the host with no bus between them, so controllers don't notice your process.

### The stack

The Matter stack is Oikade's own, in `internal/matter`, built from the Matter Core Specification: TLV, the message layer with MRP over UDP, PASE and CASE, operational certificates, the Interaction Model, the commissioning clusters, attestation with connectedhomeip's test DAC, and an mDNS responder. `internal/matter/node` wires them into a node serving a bridge.

Primitives, certificates and setup payloads are checked against published vectors. PASE, CASE with its resumption, and the encrypted message are checked against handshakes recorded with chip-tool, which connectedhomeip's own tests don't pin (`internal/matter/securechannel/testdata`, OIK-306).

### The bridge in the host

`internal/host/matter.go`'s `matterBridge` wraps `matterbridge.Bridge`, a `driver.Sink` of the hub, and calls its `Apply` with what the registry says whenever the registry or a device's discovery changes.

- **A bridged device's `UniqueID`** comes from the bridge's unique ID and the endpoint, because the endpoint is what follows a device through a rename or a replacement.
- **What's on the bridge** is what `api.Bridgeable` says: paired, not forgotten, and with a device type. The bridge, `oikade list devices` and a deploy's `takenOffMatter` all use it, and a test keeps it equal to `matterbridge.Supported`. Devices of plugins the home hides from Matter stay off too, and keep their endpoints.
- **The home's Matter options** (the hidden plugins and the bridge's name) come in the automations process's `hello`. `homeOptions` saves them in the store's `options` namespace before they're applied, so a restart applies the last deploy's before a process connects. While a deploy settles, its `hello`'s hidden plugins are ignored, and `deployer.hide` takes them from the manifest once it's confirmed, so a version that fails costs controllers nothing.
- **Matter's state** is the store's `matter` namespace: the `identity`, written once before a controller can see it, and the `fabrics`. `fabric.Table` saves through `fabric.Store`, so a fabric is on disk before commissioning hears it worked.
- **Unreadable Matter state leaves the bridge broken for good,** with no node and nothing retried, and the home running. A new identity would lose every controller silently, so `oikade status matter` and the log say why instead.
- **A bridge that can't start or stops serving,** as with a taken port, tries again every `matterRetry`. `node.Run` can run again after it returns.

### The mapping

`internal/matterbridge` makes each device on record with an endpoint ID one bridged endpoint, with a device type per function and their clusters, and Power Source for a battery.

- **A device that's on or off** is what `internal/devicetype` says, the one answer `oikade list devices` reads too: the registry's `deviceType`, which a declaration's `Is` sets, else its `driverDeviceType` from the driver, else a dimmable light with `brightness` and a plug-in unit without (OIK-142). Level Control comes with a dimmable type, not with `brightness` alone.
- **A device with a `setpoint`** is a Thermostat with only the Heating feature, whose Thermostat cluster carries its `on` and `temperature` too (OIK-20).
- **A device with a `mode` and an `on` as well** is a Room Air Conditioner (`devicetype.ClimateOf`): a Thermostat with Heating, Cooling and Auto and a dead band of 0, so both occupied setpoints are its `setpoint`, and an On/Off. Its SystemMode is `on` and `mode` together, so a controller's one write can be two (OIK-263).
- **A Door Lock's LockState follows the lock's reports,** never its commands, so a controller can't show a bolt that hasn't moved. A Window Covering's Current position does the same, and its Target comes from the command. A move shows as moving until a report reaches the target or `position`'s settle has passed on the bridge's clock, and a write that's refused cancels it (`Mapped.Cancel`, OIK-23).
- **A change of capabilities or device type** rebuilds the device on the same endpoint ID. A new name isn't a rebuild: it sets the Bridged Device Basic Information's NodeLabel in place, and subscribers are sent it (OIK-130).
- **What a device is** comes from the registry, which keeps what the driver last said (`driver.Product`, OIK-349). The Bridged Device Basic Information serves it as VendorName, ProductName and SerialNumber, cut to 32 bytes, and SoftwareVersionString and HardwareVersionString, cut to 64, each once it's known.
- **State reaches attributes under the node's lock,** and availability is the Bridged Device Basic Information's Reachable.

### Writes from controllers

- **They're coalesced** (`matterbridge.Coalescer`). Per device and capability, the last command in a 50 ms window wins, and one that matches the device's state is dropped.
- **A capability that travels,** as a blind's `position` does, gathers until 1.5 s after the last write instead, because the Home app sends a slider's drag as it goes, and each write would stop the motor to start the next. A StopMotion ends its burst at once (`Coalescer.Send`, OIK-337).
- **A device's bursts go one at a time, in order,** so a slow device can't take an older command after a newer one. Within one, a value goes before an `on` and after an off, so a SystemMode's `mode` goes first.
- **`matterWrites` arbitrates them.** It refuses against a Safety claim, on disk or in an arbiter, then records the write, so the device's change counts as `Matter`.
- **A Window Covering logs each command** a controller sends, with its target, as `Took Matter command`, so the log says what was asked as well as what was written (OIK-337).

### Commissioning

`internal/matter/clusters` owns every commissioning window, including the initial one the node opens while it's on no fabric, which closes after 15 minutes, on RevokeCommissioning, or once commissioned.

- **`oikade pair matter`** reaches `node.OpenWindow` through the host API. While the node is on no fabric that's the initial window again. Once it's on one, it's an enhanced window, with a passcode, salt and discriminator made for it.
- **It replaces only a window the node or its owner opened,** never a controller's, and waits for the commissioning hooks, so PASE takes the new verifier before it answers.
- **`internal/qr` draws the QR code:** alphanumeric mode, level M, versions 1 to 6, as a Matter payload needs no more.
- **The node advertises each fabric's operational service** when AddNOC or UpdateNOC changes it, and again when the fail-safe rolls it back.
- **Ending a session sends CloseSession,** and a peer's CloseSession ends its session.

### Subscriptions

`internal/matter/im` reports a change at most every min interval, the subscriber's floor, and sends an empty report when nothing has changed for the max interval.

- **The bridge grants a max of 60 s,** or the subscriber's ceiling if that's shorter, but never below its floor, which the spec's bounds for a publisher allow (Core spec 8.5). Apple's home hub asks for 10 minutes, and was seen to ignore CloseSession and subscribe again only once a report was late, so this bounds how long it takes to come back after a restart: about max(floor, 60 s), plus some seconds (OIK-209).
- **Each subscription over CASE is saved** after its Subscribe Response, with the subscriber's node ID, paths, intervals and last address, never keys (`internal/host/matter_subscriptions.go`). Its record goes when the client replaces it, refuses a report with INVALID_SUBSCRIPTION, or its fabric is removed, and when another subscription is saved after it has ended. It stays when the host stops, which is what it's for.
- **At each start the node resumes them** (`internal/matter/node/resume.go`), as the reference SDK, matter.js and rs-matter do. For each record it finds the subscriber by operational discovery, else at its last address, opens CASE to it as the initiator, sends a priming report under the old subscription ID, then runs it as if just subscribed.
- **It tries again after 1, 2, 4… seconds,** and deletes the record once the next wait would pass one max interval, by when the subscriber has noticed and will subscribe again itself, or at once if it refuses.
- **A new subscription ID is never one that's saved,** and a client that subscribes afresh before its old one is resumed replaces that one too.

### mDNS

The responder (`internal/matter/mdns`, OIK-93) is built on `internal/mdns`'s sockets. It also finds a node by operational discovery: a one-shot lookup of its `_matter._tcp` instance on the responder's own sockets (OIK-209).

- **It follows the interfaces.** Every five seconds it looks at them, only those `--matter-interfaces` names if it's set. It joins the mDNS groups on one that comes up, and announces the host's addresses again when they change, with a goodbye to the old ones, so a new DHCP lease doesn't leave controllers dialling a stale address for the two minutes an address record lives.
- **Its replies carry an NSEC record** for each of its names, listing the types the name has, so a querier that asks for one it hasn't, such as an AAAA on an IPv4-only link, hears so at once.
- **Two hosts probing one name break the tie** as RFC 6762, 8.2 says: the one whose records sort later goes on, and the other waits a second and probes again, when the winner answers it as a conflict.
- **A name stays defended once it's announced** (RFC 6762, 9). Another host's answer with other data for it puts it back to probing, unanswered for meanwhile, and a service whose name is lost is no longer advertised.
- **A lost name is replaced or retried.** The commissioning window is advertised under a new random instance name. An operational name, the fabric's and the node's IDs, has no other, so the node logs it and tries it again after a minute, then twice as long each time. The host name, the hardware address, is probed again too, but kept.
- **Fifteen conflicts within ten seconds** make each probe wait five seconds first (8.1).

### Panics

Matter parses whatever anyone on the network sends, so a bug there costs the message and its session, not the home (`internal/matter/contain`, OIK-172).

- **It's the one place the host recovers from a panic,** and only for work whose state goes with it: the transport's read of each packet, each handler on its exchange, each subscription's reports, and the mDNS responder's answer to a packet. Each defers `contain.Recover` itself, since a recover covers only its own goroutine.
- **A panic ends the session it came on,** with CloseSession on a secure one, so the controller starts again. A handshake that panics takes out the secure session it had added.
- **The locks those paths take go with `defer`,** the Node's included, and what an Update changed before a panic is still announced, so a contained panic can't leave a lock held and turn a crash into a hang.
- **Some state can't be left half moved.** The commissioning clusters' commands and writes defer `contain.Critical`, which `Recover` passes on, and timers and commissioning hooks aren't contained, so a panic there stops the host, which starts again clean.
- **Each contained panic is logged** with its stack, and counted in `oikade_matter_panics_total`.

## The engine and arbitration

Design: OIK-64 (the rule model), OIK-60 (room targets) and OIK-298 (decisions). The engine is `internal/engine`, which runs in your process what `oikade` compiles from your home, and the host's arbiters.

### Rules in Go

`internal/engine/rule.go`'s `NewHold`, `NewFlow` and `NewTrigger` build the engine's input, which `oikade` calls.

- **A condition's `Vars`** carry each device's values and its room and device types (`hub.Member`), so a condition can read a room (OIK-180).
- **A hold with a condition looks again** on any change to a device, and when a device is announced again, as a move to another room or a new device type announces it.
- **A hold writes** a device, a group's members (`Devices`), or a room's devices apart from those with a decision of their own (`Except`).
- **A group's or a room's start writes the members it can,** and is active once it has reached one. A member that isn't discovered yet, or whose write failed, gets the start once it's discovered or at its retry (`Engine.joined`). That retry doesn't hold up the hold's end, and a group's end writes back only the members it wrote (OIK-321). After a restart, an active hold counts every member as written, so one whose write was still to retry isn't written until the hold starts again.

### Decisions and holds

- **A decision** compiles in `oikade` (`decide.go`) to the host's `control.Entry` for the capability, and an engine hold for each case and budget line, keyed `<device> <capability> <case>`.
- **A case above the override line,** or every case with `NoOverride`, is a Safety hold. A case's `LatchUntil` is the hold's `Until`, which ends its latch. A budget line's conditions are its hold's `When`.
- **A `Follow` line** is a hold with `Follow`, which works out its value on every look. It's active while the reading is known, rounds to `Step` or the capability's `Spec.Step`, and claims again only when the value is three quarters of a step from `HoldState.Target`, the last one written, which the host saves.
- **A group's decision** compiles to one hold per case over its members, and to a setting for each member, with a member's own lines ahead of the group's when it hands over with `Then`.
- **A room's decision** is one setting for the room (`wire.DeviceControl.Room`), which the host gives each device there of its types with the capability and no setting of its own (`wire.SettingsFor`), so a move needs no deploy.
- **Holds aren't timed in memory.** The engine works out from the clock and the devices' state whether each hold is active, and when that next changes. It saves each hold's state, so after a restart it writes only if the hold started or ended while it was down.
- **An inactive hold's `WhyNot` says why,** which your process sends the host as a `whyNot` message for `oikade status`.

### Conditions

Conditions are values in `oikade` (`condition.go`), each of which can say whether it holds, why it doesn't, and what's wrong with it for `oikade check`. `toEngine` makes one an `engine.Condition`.

- A hold's `Daily`, alone or in an `All`, becomes the engine's `Window` (`splitHours`), so it starts and ends on its edge.
- A flow's `Get` and `Is` are `look` steps in its journal, read through `Engine.Look`, so a replay returns what the run first read.
- A `Becomes` is an `engine.Change` with `To`, which fires only on a change to that value.

### Flows

Flows start when a trigger fires and the condition holds, and run in `internal/flow` ([durable flows](#durable-flows)). Times of day are timers.

- **A room target resolves to its members** when the trigger or `Set` runs, not when the rule loads, so a device added to a room is covered at once.
- **A flow's `Set` ranks** at its own `Flows(f)` line in the decision, `Rule/<flow>` in the entry's `lines`, or else at `flows` (`claimPriority`). A flow any decision ranks above its override line is Safety (`flowSafety`).
- **A flow's `Set` on a room of device types** leaves the other types out as written, and logs them once. A member it leaves out because it arbitrates the capability, with no `For` or `Until`, is the step's error.

### Rolling averages

`internal/engine/average.go`'s `engine.Average`s are what `AverageOver` made as the home was built, which `oikade` gives `Engine.Averages`.

- Each takes every report of its reading into time-weighted buckets, a window's 48th each.
- It's saved in the host's `averages` namespace through `saveAverage` when a report lands in a new bucket, and every changed one is saved when the engine stops.
- A condition reads one as a capability of its device, `temperature over 24h`, which the engine adds to the devices it looks at. A budget's `Bands` read it the same way.

### The day planner

`internal/schedule` lays out a day in slots of 15, 30 or 60 minutes, 30 unless a budget's `SlotMinutes` says, which rules require, forbid or prefer, and spends a daily budget across them. A budget is a `schedule.Spec`, and its plan lives in `HoldState.Budget`, so it survives a deploy.

### A report is applied as a whole

Design: OIK-212.

A driver passes every value one report from a device changed to `driver.Sink.Changed` in one call. The store takes them all in before its listeners hear, the host sends them to your process as one `change`, and the engine handles them as one event. So every hold looks at the whole report.

The writes the holds start in one look, a report or a timer, then run in order (`Engine.startWrites`):

1. **Claims that start go before claims that end,** so a line that takes over from another at the same moment has claimed before the other lets go, and the arbiter never falls back to the default on the way (OIK-343).
2. **Then claims go highest level first,** so a Safety claim is in before a Normal one from the same report, and the arbiter never writes the Normal value, even for a moment.
3. **Plain writes, with no `Control` to arbitrate them, go the other way:** ends first, since a bool hold's end writes the opposite of its value, then lowest level first, so a start's value, and the highest level's, is the one that stays.

Only writes that can reach the same device wait for each other: in one room, or to one device with no room, and a write to a group's members joins the rooms they're in. A flow the same look sets off starts once those writes are in. `oikade replay` puts a device's changes at the same moment back into one report.

### Arbitration

`internal/control` decides an arbitrated capability.

- **Claims have a level:** Safety, Override or Normal. Rules, Matter controllers and the device itself make them. The highest level wins.
- **Within a level, the entry's `lines` rank claims,** highest first. Each is a rule's source, `override` for an outside change, or `flows` for any rule not named, and the later claim wins a tie. Then the equipment limits hold the winner back.
- **A controller's write reaches the engine in the host,** so it becomes a `Matter` claim.
- **Sources keep their stored form.** The engine and the store keep a claim's source as `Rule/<key>`, `Matter`, `Device` or `Command`, and a rule's key as `<device> <capability> <case>` or a flow's name, so saved claims, pauses and lines don't move. The host API and `internal/simhome` give it as its ref (`control.SourceRef`, and `control.RuleKey` back), whose grammar is `internal/ident`'s `ParseRef`.
- **A write settles before a report counts.** A report that differs from the arbiter's last write is ignored while the write settles, for `capability.SettleOf`: the capability's `Spec.Settle` (2 min for `position`, 30 s for `locked`) or `DefaultSettle` (5 s). The device's value is looked at again once that's up, so a blind's travel isn't an outside change, and one that stopped partway is. A write from Matter or `oikade set` settles the same way, except that the report it's waiting for counts at once, and it matches the device's change for 25 s after it has settled.
- **A restarted arbiter trusts the first report.** An arbiter that starts from a saved state, as after the host starts, takes the device's first report as what the device is now, never as an outside change. It does the same when a driver gets a new process, and a write may not have been made (`Engine.DriverReturned`). So whether a report or a deploy's removals come first at startup doesn't decide an Override (OIK-318), and the arbiter writes the winner again if the device disagrees.

## Simulated homes and replay

### `simhome`

`internal/simhome` runs a home in one process: one `Engine` with both rules and arbiters on a fake clock, with its states and journal in memory. `oikade dev` is your binary's `dev` role reading commands from standard input, and `oikadetest` is the same thing in a test. `oikade` fills in `simhome.FromHome` when it loads, because a public package can't name a home's compiled form.

`Sim.settle` needs to know the engine has caught up, and `Engine.Idle` says so exactly:

- The engine counts what it has in flight: changes queued, a wake or save asked for, and every goroutine it starts.
- A run stops counting while it waits through `Engine.Park`, as the engine's own waits and a flow's do. A parked run whose time has come, or whose change has happened, isn't waiting any more.
- A run cancelled mid-wait is busy until it ends, a failed save is owed until one succeeds, and the engine knows when its own timer is due.

`settle` doesn't poll `Idle`. It checks again when `Engine.MaybeIdle` says the count has fallen to zero or the timer was set (OIK-314). `Engine.ParkedUntil` says when the next parked run wakes, so `Advance` can move straight to it. A restart builds a new engine from the states and journal, as a new process would, so `flow.Recover` runs for real.

### `oikade replay`

`oikade replay` is a `Sim` too (`internal/simhome/replay.go`). The command builds your module and hands its binary a file with the host's event log, and the binary runs the log through a `Sim` and prints a report, so the host never runs your working copy.

- **Echoes don't go in.** Changes within 10 seconds of a write to the same capability are the device answering it. Otherwise the deployed version's writes would happen whatever your code does.
- **Only your automations' writes count,** here and in the comparison. One whose `by` is `matter` or `command` is a person's, which a replay doesn't make, so what it changed goes in as a change from outside (OIK-355).
- **A device's first value starts it off,** rather than going in as a change.
- **The start follows the log.** If the log begins with the deployed version's process connecting, your code starts when it did, with what the devices said before that as how they were, and what each wrote on starting is compared. Otherwise your code starts a second before the first event, and neither's writes on starting count (OIK-320).
- **Flows' outside calls don't run.** `Flow.Dry` answers each from the log, by name and in order.

## The store and the event log

### The store

`internal/store` is the host's state store.

- **One file.** A data directory holds one [bbolt](https://github.com/etcd-io/bbolt) file, `oikade.db`, and only one process can have it open. The operating system drops that lock when the process dies, so a crash never leaves it locked, and a second process gets a clear error.
- **Namespaces** share the file with separate keys: the registry, latches, claims and Matter state, its saved subscriptions among them. `engine.Stored` implements `engine.States` on a namespace.
- **The journal** keeps ordered entries per run. `Append` returns only once its entry is on disk. Each write is one transaction with two fsyncs, so its cost is about twice the disk's fsync time. A crash mid-write loses only the entry that was never acknowledged.
- **The format is checked.** The file records its format version, and `Open` refuses one it doesn't know. It also refuses, with the same `ErrFormat`, a file that isn't bbolt's, or one shorter than its meta page says, which it checks before bbolt maps it, because bbolt reading past the end of a file cut short faults the process (OIK-307).

### The event log

The event log (`store.Events`, fed by `internal/host/eventlog.go`) is what `oikade replay`, `oikade history` and `oikade status REF` read (OIK-355). Beside changes, writes and runs, it has a device going offline or coming back (`hub.Store.Went`), a plugin's own problem (`Instance.OnProblem`), the Matter bridge's window and controllers (`node.Config.WindowChanged` and `FabricsChanged`), and devices forgotten and renamed (`inventory.record`), each reported where the host logs it.

It's history, not state: nothing depends on it, so it trades the rest of the store's durability for staying out of the way.

- **Reporting an event only appends to a queue.** One commit every 5 seconds takes the queue, so a crash loses at most that long. Past 4096 waiting events, new ones are dropped, not waited for.
- **An idle host doesn't sync the disk.** With nothing queued and nothing over the bound there's no commit.
- **The bound folds into a baseline.** A commit drops the oldest events over the bound, folding their last values into a baseline in the same transaction, because a device that hasn't changed since still has them.
- **Reads take pages,** each its own short transaction, because a long one would hold up writes that grow the file, journal entries among them.
- **A filtered read reads the log through.** The log has no index by time or device, so `eventMatcher` (`internal/host/eventfilter.go`) sends what passes, after one pass for the `rename` events if a ref names a device.
- **A tail that starts with the log's past has each event once.** An event goes to the log's queue and to the feed under one lock (`server.emit`), and such a tail subscribes to the feed at the moment a flush takes the queue (`eventLog.flushedAt`). So the log up to that flush's last record and the feed after it are every event, and neither waits for the disk with that lock held.

#### What it's sized on

The default bound is 14 days or 64 MiB, whichever comes first (`--event-log-age`). It's sized on an estimated home of 50 devices at about 24,000 events a day, which holds 14 days in about 35 MiB and a 48 MiB file, so the size bound has room for a home nearly twice as busy. That estimate is `aHouse`, in `internal/host/eventlog_test.go`, and no real home has measured it yet.

An event is kept without a ref that only repeats its device, capability or rule, which the host works out again as it reads (`encodeEvent`, `decodeEvent`). That's a third of a change's size, and more than keeping the value before it costs.

With a full 64 MiB log and a commit every 20 ms, the journal's median append went from 9.0 to 10.0 ms on a Mac's SSD, measured once by hand. Timings vary too much to test, so `TestAFullLogDoesntSlowTheJournal` checks what they allocate instead: a log commit at most a page more in a log four times the size, and a journal append at most a page more with a full log than with none.

## Releases

Design: OIK-63 (release automation) and OIK-170 (the two binaries).

release-please keeps a release PR open on main. Merging it tags the release, and `.github/workflows/release.yml` runs the tests, lint and journeys on that commit, then GoReleaser (`.goreleaser.yaml`) on the tag. [CONTRIBUTING.md](../CONTRIBUTING.md) has what a release ships, and how a milestone is cut.

- **The image holds the archive's binary.** The Dockerfile copies the prebuilt `oikade-host` rather than compiling it.
- **Everything is built with cgo off,** so it's static and runs on distroless.

### Dialling `.local` names

On Linux, cgo off means Go's own resolver, which never does mDNS. So wherever Oikade dials an address you give it (an Overkiz hub, an MQTT broker, `oikade`'s host), it dials through `internal/mdns`'s `DialContext` (OIK-328, OIK-360):

- It looks a `.local` name up over mDNS, and with the system's resolver only if nothing answers there, since a router's DNS may answer for any name.
- It looks a name up again after a minute. If nothing answers then, as a host's responder misses the odd query, it dials the addresses it found before, until none of them answers.
- `internal/mdns` is mDNS's sockets and nothing of Matter, so `oikade` links it too. The Matter responder, `internal/matter/mdns`, is built on it.
