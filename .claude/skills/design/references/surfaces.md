# Oikade surfaces and invariants

This is the checklist the `design` and `plan` skills use. Oikade is small, but every feature touches some of the things below. The expensive mistakes come from changing one of them without noticing. Read the code to confirm details, because this file summarises it and can go stale. Check `README.md` and `framework` before relying on a specific field name.

## Contents

1. Who owns what
2. Public API: things users depend on
3. State that must survive
4. Component boundaries
5. Hardware and environment
6. Verification layers
7. Standing preferences

## 1. Who owns what

| Owner | Holds |
|---|---|
| Git, as a Go module | A home's automations, and a copy of its device list from `oikade types` |
| The host's data directory | Devices, names, rooms, endpoints, claims, latches, journals, deployed versions, Matter's identity, secrets |
| Prometheus, Grafana, Loki | Values over time, changes, rule runs |

Ask these questions of every feature:

- **Is it code or state?** What a home does belongs in its module, and is reviewed in Git. What a home is, right now, belongs to the host.
- **Who owns naming?** The registry owns Oikade's names, and each app owns its own ([OIK-130](https://linear.app/oikade/issue/OIK-130)). Drivers address devices by ID, so a rename in an app changes nothing in Oikade, and Oikade never writes an app's names.
- **Does it survive a deploy, a crash of your process and a crash of the host?** Those are three different answers.

## 2. Public API: things users depend on

These appear in users' Git repos, dashboards, alert rules and Home app setups. Changing them breaks someone, so a design has to call out any change to them explicitly. Nothing here needs a migration while Oikade is greenfield, but a design still says what changes.

- **`framework`,** the Go API a home is written in. `framework/hometest` too.
- **The `oikade` commands, their flags and their `--json`.** Scripts and a home's CI use them.
- **Capability names and value semantics** (`internal/capability`). These include units, range and polarity: `sensor.contact-open: true` means open. They appear in a home's code and in PromQL. The `add-capability` skill owns this procedure.
- **Metric names and labels** (`oikade_*`). Dashboards and alerts query them. Adding a label changes series identity.
- **The plugin contract** (`docs/plugin-contract.md`, `contract/v1`, `sdk/plugin`) **and `oikade-conformance`'s name and flags.** Plugin repos depend on them through their `go.mod` and CI, and each plugin is released on its own schedule. A change reaches a Go plugin when it bumps `oikade`, and a plugin in another language when it changes the version it pins in `go install …@vX.Y.Z`.
- **The host protocol** (`docs/host-protocol.md`) between the host and a process built against another version.
- **Log messages that are queried**, for example `"Changed capability"` in Loki.
- **Matter endpoint IDs.** They're append-only in the registry: never reused or reordered, and kept for removed devices. Controllers key rooms, scenes and automations on them.

## 3. State that must survive

Losing any of these means re-pairing. Then the Home app forgets rooms, scenes and automations, or every Zigbee device has to be paired again. A feature that touches one of them needs an explicit "what happens on upgrade, delete, and restore" answer.

- The host's data directory: the Matter bridge's identity and fabrics, the registry with its endpoint IDs, claims, latches and journals.
- Endpoint ID allocations: never reused or reordered, and kept for removed devices.
- The Zigbee network key and database, which belong to the user's Zigbee2MQTT, and aren't Oikade's to touch.
- Anything new that would be painful to lose goes in the store, and is listed in README "State that must survive" and in the Pi guide's backup section.

## 4. Component boundaries

The host, your process, plugins and the data path are in `docs/architecture.md`. The boundaries a design must not cross without saying so:

- **The host** is one process per home, because two would run every rule twice. A feature that assumes high availability, or needs leader election, is a big decision.
- **Your process** can die at any moment. Whatever must survive that lives in the host, and everything that changes the host's state is a request that waits for its answer.
- **Matter runs in the host,** so controllers keep working through a deploy or a crash of your process.
- **A plugin** is a process of its own. A new driver is a plugin unless there's a reason it can't be.
- **`framework`, `contract/v1` and `sdk/plugin`** are public, and can't mention `internal/`.

## 5. Hardware and environment

- The target is **one small arm64 or amd64 Linux machine**, such as a Raspberry Pi, on systemd or in Docker. Memory and CPU are tight. There's no Kubernetes target.
- The Matter bridge needs **L2 adjacency, mDNS and IPv6** with controllers. In-process node tests and chip-tool cover the protocol; how the Home app shows things can only be checked manually, against real devices.
- The user runs Zigbee2MQTT and an MQTT broker. The host only connects to them.

## 6. Verification layers

Say which layer proves each behaviour. Don't just write "add tests".

| Layer | Command | Proves |
|---|---|---|
| Unit | `go test ./internal/...` | Registry, z2m exposes against fixture JSON, the Matter mapping and coalescer, engine timing with a fake clock, Matter commissioning in an in-process node test |
| Integration | `make test` (`test/host`, `test/cli`) | A real host and process with simulated devices: crashes, deploys, commands, plugins |
| Conformance | `make test-conformance` | The plugin contract, against the simswitch fixture |
| Install | `make test-install` | `oikade install` and `upgrade` in a container with systemd |
| Journeys | `make test-journeys`, on main and nightly only: agents don't run them | From `oikade init` to a running host, and chip-tool as the controller |
| Manual, in the Manual testing doc | Real devices and the Home app | Pairing with the Home app, real Zigbee payloads, the Home app's rendering, arm64 resource use |

Lint with `make lint`.

## 7. Standing preferences

- **No Home Assistant as a driver** or component. Standalone device libraries that Home Assistant also uses are fine inside plugins. Prefer MQTT-native projects (ESPHome, Tasmota, Shelly, zigbee2mqtt), Matter, or plugins, written in Go first ([OIK-62](https://linear.app/oikade/issue/OIK-62)): first-party ones in `plugins/<name>` ([OIK-155](https://linear.app/oikade/issue/OIK-155)), and anyone else's in its own repo.
- The user is an SRE. Reviewability, observability and boring operability beat cleverness.
- Log messages follow the Kubernetes style: capitalised, past tense, no trailing period.
