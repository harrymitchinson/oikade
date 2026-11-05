# Oikade surfaces and invariants

This is the checklist the `design` and `plan` skills use. Oikade is small, but every feature touches some of the things below. The expensive mistakes come from changing one of them without noticing. Read the code to confirm details, because this file summarises it and can go stale. Check `README.md` and `api/*/v1alpha1/*_types.go` before relying on a specific field name.

## Contents

1. The three planes
2. Public API: things users depend on
3. State that must survive
4. Component boundaries
5. Hardware and environment
6. Verification layers
7. Scaffolding rules
8. Standing preferences

## 1. The three planes

| Plane | Owner | Holds |
|---|---|---|
| Control | Git, as CRDs (`home.oikade.dev`, `matter.oikade.dev`, `zigbee.oikade.dev`, `plugins.oikade.dev`) | What should exist: Devices, Rooms, Rules, Bridges, Zigbee2MQTT instances |
| Data | MQTT (Mosquitto) | Live state and commands: "light is on", "21.4°C", `/set` |
| History | Prometheus, Grafana, Loki | Values over time, changes, rule runs |

Ask these questions of every feature:

- **Is it config or state?** Config belongs in a CRD and is reviewed in Git. Live state never goes into etcd. `Device.status` is deliberately coarse: capabilities, conditions, and `lastSeen` throttled to 5 minutes. If a design writes fast-changing values to status, it breaks this rule.
- **Who owns naming?** Git owns Oikade's names (`metadata.name`, `displayName`), and each app owns its own ([OIK-130](https://linear.app/oikade/issue/OIK-130)). Drivers address devices by ID, so a rename in an app changes nothing in Oikade, and the operator writes no names into apps. It still renders zigbee2mqtt's `configuration.yaml` and overwrites it on every start; device names live in `devices.yaml` on the PVC, which it only seeds.
- **Is drift reported?** Something that exists but isn't declared, such as a paired but undeclared device, becomes a metric and a log line. It is never silently adopted.

## 2. Public API: things users depend on

These appear in users' Git repos, dashboards, alert rules and Home app setups. Changing them breaks someone, so a design has to call out any change to them explicitly.

- **CRD fields and enums.** They're `v1alpha1`, but people's homes are declared in them. Prefer additive, optional fields with defaults. A rename or a change of meaning needs a migration note.
- **Capability names and value semantics** (`internal/capability`). These include units, range and polarity: `sensor.contact-open: true` means open. They appear in Rule YAML, PromQL and `Device.status`. The `add-capability` skill owns this procedure.
- **Metric names and labels** (`oikade_*`). Dashboards and alerts query them. Adding a label changes series identity.
- **The CEL environment** for Rules: `trigger`, `devices`, `now`, `tz`. Its variables and types are a language contract.
- **The plugin contract** (`docs/plugin-contract.md`, `contract/v1`, `sdk/plugin`) **and `oikade-conformance`'s name and flags.** Plugin repos depend on them through their `go.mod` and CI, and each plugin is released on its own schedule. A change reaches a Go plugin when it bumps `oikade`, and a plugin in another language when it changes the version it pins in `go install …@vX.Y.Z`.
- **Log messages that are queried**, for example `"Changed capability"` in Loki.
- **Object names and labels**: `<bridge>-matter` (Secret and PVC), `<instance>-network`, `<instance>-data`, and the labels `matter.oikade.dev/bridge` and `zigbee.oikade.dev/instance`. Backup and cleanup docs depend on them.
- **Matter endpoint IDs.** They're append-only in `Bridge.status.endpoints`, with a copy in the `<bridge>-matter` Secret: never reused or reordered, and kept for deleted Devices. Controllers key rooms, scenes and automations on them.

## 3. State that must survive

Losing any of these means re-pairing. Then the Home app forgets rooms, scenes and automations, or every Zigbee device has to be paired again. A feature that touches one of them needs an explicit "what happens on upgrade, delete, and restore" answer.

- The Matter bridge's `<bridge>-matter` Secret (setup code, unique ID, attestation key, and the endpoint ID backup) and its PVC (the fabric table). Neither has an owner reference.
- Endpoint ID allocations: never reused or reordered, and kept for deleted Devices.
- The Zigbee network key Secret and the z2m database PVC. Neither has an owner reference.
- Anything new that would be painful to lose follows the same pattern: no owner reference, a predictable name and label, restorable from status or Git where possible, and listed in README "State that must survive" and in the Pi guide's backup section.

## 4. Component boundaries

The roles (`manager`, `manager runtime`, `manager matter`), the data path and the drivers are in `docs/architecture.md`. The boundaries a design must not cross without saying so:

- **`manager`** only talks to Kubernetes, never to MQTT.
- **`manager runtime`** is exactly one replica, because two would run every rule twice. A feature that assumes runtime HA, or needs leader election, is a big decision.
- **`manager matter`** keeps controllers working while the runtime is down. It has **read-only** API access and can read no Secrets: its own are mounted.
- **A new driver** (ESPHome, say) reports to a `driver.Sink`, takes writes as a `driver.Setter`, and joins the Device spec's driver union.

## 5. Hardware and environment

- The real target is the user's **arm64 Raspberry Pi cluster** running k3s. Images must build for arm64, and memory and CPU are tight.
- The Matter bridge needs **L2 adjacency, mDNS and IPv6** with controllers. In-process node tests and chip-tool cover the protocol; how the Home app shows things can only be checked manually, against real devices.
- The Zigbee coordinator is either networked (`tcp://host:6638`), so the pod can run anywhere, or USB, so the pod is pinned with `nodeSelector` and runs privileged.
- Mosquitto is anonymous inside the cluster today. MQTT auth and TLS are listed under "Not yet".

## 6. Verification layers

Say which layer proves each behaviour. Don't just write "add tests".

| Layer | Command | Proves |
|---|---|---|
| Unit | `CGO_ENABLED=0 go test ./internal/...` | Registry, z2m exposes against fixture JSON, the Matter mapping and coalescer, CEL compile and evaluation, engine timing with a fake clock, Matter commissioning in an in-process node test |
| Integration | Part of `make test`, with an in-process broker (`internal/mqtt/mqtttest`) | Bus → status, metrics, and engine `/set` |
| envtest | `CGO_ENABLED=0 make test` | Controllers: endpoint ID stability, the Secret backup, rendered z2m config, `Ready=False` on a bad CEL |
| Kind e2e | `make test-e2e`, on main and nightly only: agents don't run it | A few journeys across the deployed system, with z2m faked by `mosquitto_pub` and chip-tool as the controller |
| Manual, in the Manual testing doc | Real devices and the Home app | Pairing with the Home app, real Zigbee payloads, the Home app's rendering, arm64 resource use |

Lint with `GOTOOLCHAIN=go1.26.0 make lint`.

## 7. Scaffolding rules (from AGENTS.md)

- New kinds come from `kubebuilder create api`, and webhooks from `kubebuilder create webhook`. Never create those files by hand.
- After editing types or markers, run `make manifests generate`. Never edit `config/crd/bases`, `config/rbac/role.yaml`, `zz_generated.*` or `PROJECT`.
- Multigroup layout: a new driver gets its own API group, as `zigbee.oikade.dev` does.
- Log messages follow the Kubernetes style: capitalised, past tense, no trailing period.

## 8. Standing preferences

- **No Home Assistant as a driver** or component. Standalone device libraries that Home Assistant also uses are fine inside plugins. Prefer MQTT-native projects (ESPHome, Tasmota, Shelly, zigbee2mqtt), Matter, or plugins, written in Go first ([OIK-62](https://linear.app/oikade/issue/OIK-62)), each in its own repo ([OIK-108](https://linear.app/oikade/issue/OIK-108)).
- The user is an SRE. GitOps reviewability, observability and boring operability beat cleverness.
