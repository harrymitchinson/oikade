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
| Control | Git, as CRDs (`home.oikade.dev`, `homekit.oikade.dev`, `zigbee.oikade.dev`) | What should exist: Devices, Rooms, Automations, Bridges, Zigbee2MQTT instances |
| Data | MQTT (Mosquitto) | Live state and commands: "light is on", "21.4°C", `/set` |
| History | Prometheus, Grafana, Loki | Values over time, changes, automation runs |

Ask these questions of every feature:

- **Is it config or state?** Config belongs in a CRD and is reviewed in Git. Live state never goes into etcd. `Device.status` is deliberately coarse: capabilities, conditions, and `lastSeen` throttled to 5 minutes. If a design writes fast-changing values to status, it breaks this rule.
- **Who owns naming?** Git does. The operator renders zigbee2mqtt's `configuration.yaml` from Devices and overwrites it on every start. Changes made in a frontend are drift, not configuration.
- **Is drift reported?** Something that exists but isn't declared, such as a paired but undeclared device, becomes a metric and a log line. It is never silently adopted.

## 2. Public API: things users depend on

These appear in users' Git repos, dashboards, alert rules and Home app setups. Changing them breaks someone, so a design has to call out any change to them explicitly.

- **CRD fields and enums.** They're `v1alpha1`, but people's homes are declared in them. Prefer additive, optional fields with defaults. A rename or a change of meaning needs a migration note.
- **Capability names and value semantics** (`internal/capability`). These include units, range and polarity: `sensor.contact-open: true` means open. They appear in Automation YAML, PromQL and `Device.status`. The `add-capability` skill owns this procedure.
- **Metric names and labels** (`oikade_*`). Dashboards and alerts query them. Adding a label changes series identity.
- **The CEL environment** for Automations: `trigger`, `devices`, `now`, `tz`. Its variables and types are a language contract.
- **Log messages that are queried**, for example `"Changed capability"` in Loki.
- **Object names and labels**: `<bridge>-identity`, `<bridge>-hap`, `<instance>-network`, `<instance>-data`, and the labels `homekit.oikade.dev/bridge` and `zigbee.oikade.dev/instance`. Backup and cleanup docs depend on them.
- **HomeKit instance IDs.** AIDs are append-only in `Bridge.status.accessories`. Service and characteristic IIDs are positional: `hap.NewServer` numbers them in the order they're added. Reordering silently remaps users' Home app scenes.

## 3. State that must survive

Losing any of these means re-pairing. Then the Home app forgets rooms, scenes and automations, or every Zigbee device has to be paired again. A feature that touches one of them needs an explicit "what happens on upgrade, delete, and restore" answer.

- HomeKit identity Secret and the pairings PVC. Neither has an owner reference. The Secret is restored from `Bridge.status.username`.
- AID allocations: never reused or reordered, and kept for deleted Devices.
- The Zigbee network key Secret and the z2m database PVC. Neither has an owner reference.
- Anything new that would be painful to lose follows the same pattern: no owner reference, a predictable name and label, restorable from status or Git where possible, and listed in README "State that must survive" and in the Pi guide's backup section.

## 4. Component boundaries

There is one binary with three roles:

- **`manager`**: the operator. It only talks to Kubernetes, and never to MQTT.
- **`manager runtime`**: exactly one replica, because two would run every automation twice. It runs the drivers on the bus, the Device status writer, `/metrics` on `:8080`, and the CEL automation engine. A feature that assumes runtime HA, or needs leader election, is a big decision.
- **`manager hap --bridge <name>`**: one per Bridge, with `hostNetwork`. It follows the bus itself so HomeKit keeps working if the runtime is down. It has **read-only** API access and can only read its own Secrets.

The data path is: driver ↔ MQTT ↔ hub ↔ {status writer, exporter, engine, HAP}. Writes go through `driver.Set`. A new driver (ESPHome, Matter, Homebridge) implements `internal/driver.Driver` and joins the Device spec's driver union.

## 5. Hardware and environment

- The real target is the user's **arm64 Raspberry Pi cluster** running k3s. Images must build for arm64, and memory and CPU are tight.
- HAP needs **L2 adjacency and mDNS** with Apple devices. Kind on macOS can't do mDNS, so anything user-visible in HomeKit can only be checked manually, against real devices and the Home app.
- The Zigbee coordinator is either networked (`tcp://host:6638`), so the pod can run anywhere, or USB, so the pod is pinned with `nodeSelector` and runs privileged.
- Mosquitto is anonymous inside the cluster today. MQTT auth and TLS are listed under "Not yet".

## 6. Verification layers

Say which layer proves each behaviour. Don't just write "add tests".

| Layer | Command | Proves |
|---|---|---|
| Unit | `CGO_ENABLED=0 go test ./internal/...` | Registry, z2m exposes against fixture JSON, HAP mapping, CEL compile and evaluation, engine timing with a fake clock |
| Integration | Part of `make test`, with an in-process broker (`internal/mqtt/mqtttest`) | Bus → status, metrics, and engine `/set` |
| envtest | `CGO_ENABLED=0 make test` | Controllers: AID stability, identity restore, rendered z2m config, `Ready=False` on a bad CEL |
| Kind e2e | `make test-e2e` | The deployed system with z2m faked by `mosquitto_pub` |
| Manual, in #27 | Real devices and the Home app | HomeKit pairing, real Zigbee payloads, the Home app's rendering, arm64 resource use |

Lint with `GOTOOLCHAIN=go1.26.0 make lint`.

## 7. Scaffolding rules (from AGENTS.md)

- New kinds come from `kubebuilder create api`, and webhooks from `kubebuilder create webhook`. Never create those files by hand.
- After editing types or markers, run `make manifests generate`. Never edit `config/crd/bases`, `config/rbac/role.yaml`, `zz_generated.*` or `PROJECT`.
- Multigroup layout: a new driver gets its own API group, as `zigbee.oikade.dev` does.
- Log messages follow the Kubernetes style: capitalised, past tense, no trailing period.

## 8. Standing preferences

- **No Home Assistant as a driver** or component. Standalone device libraries that Home Assistant also uses are fine inside plugins. Prefer MQTT-native projects (ESPHome, Tasmota, Shelly, zigbee2mqtt), Matter, or plugins, written in Go first ([0006](../../../../docs/design/0006-go-plugin-sdk.md)).
- The user is an SRE. GitOps reviewability, observability and boring operability beat cleverness.
