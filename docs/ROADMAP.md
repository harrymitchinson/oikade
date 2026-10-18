# Oikade roadmap

_Updated 2026-10-18._ How to change this: edit it in a PR. Specs live in [docs/specs](specs/). Work is tracked in Backlog.md, with one milestone per section below.

## Direction

Oikade runs the whole house from Git: every device declared, HomeKit through its own bridge, and history in Prometheus. Next it gains a plugin system, so that non-Zigbee device families (the SONOFF relays, the pool pump, the Cozytouch heaters) can join without core changes, and schedules that can run the pool. Then Homebridge can be retired. It is not a general-purpose hub: it has no Home Assistant, no UI for configuration, and no cloud.

## Now: Pi bring-up

**Goal:** the real house runs on Oikade on the Pi cluster, not just in Kind.

**Done when:**

- `kubectl -n home get devices` on the Pi shows every declared Zigbee device with `Resolved=True` and `Available=True`, deployed by GitOps from a CalVer release built for arm64.
- The Bridge is paired in the Home app. Toggling a light from the phone works, and it still works with the runtime scaled to zero.
- The hallway motion automation runs for a week with no manual restarts.
- Restore drill: delete the `home` namespace, restore from Git and backups, and nothing has to be re-paired. The Home app keeps its accessories and rooms, and Zigbee devices stay joined.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|
| Rebuild the cluster and deploy Oikade by GitOps (arm64 image) | — (ops, per [Pi guide](deploy-raspberry-pi.md)) | M | — | In progress |
| Pair the HomeKit bridge and declare the real Zigbee devices | — (ops) | S | Rebuild | Parked |
| Backup and restore drill (identity Secret, network key, PVCs) | — (ops) | M | Pairing | Parked |
| Check whether zigbee2mqtt re-reports occupancy while someone stays put | [0001](specs/0001-room-targeted-actions.md), open question | S | Pairing | Parked |
| Room-targeted automations (TASK-0002) | [0001](specs/0001-room-targeted-actions.md) | M | — (the occupancy check only affects README guidance) | Ready |

Room-targeted automations are here, not in Next, because they're small and improve automations that this milestone starts running for real. The occupancy check they wait on happens during bring-up anyway.

## Next: Plugin drivers

**Goal:** a new device family no longer needs a core change. Oikade has a plugin system: the contract, the `Plugin` kind, the SDK and conformance. Real device plugins come afterwards.

**Done when:**

- CI runs the plugin contract conformance suite against the simulated example plugin.
- A `Plugin` and a `Device` declared in Git, served by the example plugin, reach `Resolved=True`. They show up in Grafana, an Automation can switch them, and the Home app toggles them, even with the runtime scaled to zero.
- Writing a new plugin needs no change to `internal/hub` or `api/home`.
- The example plugin pod stays under 128 MiB RSS on arm64.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|
| Contract, driver-agnostic hub, `Plugin` API, runtime and HAP wiring (steps 1–4) | [0002](specs/0002-plugin-drivers.md) | L | Pi bring-up | Not started |
| Plugin reconciler in the manager, TypeScript SDK, conformance, Kind e2e (steps 5–7) | [0002](specs/0002-plugin-drivers.md) | M | Steps 1–4 | Not started |
| Plugin system docs (step 8) | [0002](specs/0002-plugin-drivers.md) | S | Steps 5–7 | Not started |

This comes after bring-up because the plugin contract and its Pi constraints (`hostNetwork`, multicast, Node memory) are cheaper to get right once Oikade is actually running there.

## After that: Pool on Oikade

**Goal:** the pool pump runs on an Oikade Schedule, and Homebridge is retired.

**Done when:**

- Each morning, `kubectl get schedule pool-filtration` shows a target matching the water-temperature band, with blocks in off-peak and swimming hours. Credited minutes reach the target by the end of the day, and Grafana shows both.
- Temporarily raising the frost rule's threshold turns the pump on within a minute.
- Toggling the pump in the Home app holds it for 2 hours, and the `Pool automatic` switch pauses the schedule.
- Homebridge no longer runs the pool.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|
| Schedule planner, API, validation, runner, live rules and overrides (steps 1–5) | [0003](specs/0003-schedules.md) | L | — | Accepted |
| `Pool automatic` switch in HomeKit (step 6) | [0003](specs/0003-schedules.md) | S | 0002 steps 1–4 | Accepted |
| Schedule docs, e2e and dashboard (step 7) | [0003](specs/0003-schedules.md) | S | Step 6 | Accepted |
| TH Elite plugin: relay and water temperature as capabilities | needs design (a plugin on the 0002 system) | M | Plugin drivers | Not started |

This comes after plugin drivers, because the pool needs the TH Elite plugin for its relay and probe, and the HomeKit switch reuses the plugin contract. Steps 1–5 of spec 0003 don't depend on anything, though, and the planner is the riskiest piece. So they're a good choice for spare evenings during Now or Next, and can be moved earlier if plugin work stalls.

## Later

- **SONOFF MINIR4 plugin.** It moves the hall and porch relays off Homebridge. It needs design first: where the plugin lives, and how it gets `sonoff-lan`. It's a good first real plugin because it's a plain relay. It could move into Next once the plugin system is done.
- **Cozytouch heater plugin.** It proves the contract with a cloud-backed family. It comes after the pool, which matters more in summer.
- **MQTT authentication and TLS.** The broker is cluster-internal today. Do it before anything outside the cluster connects, and give each plugin a credential scoped to its own topics.
- **Multi-endpoint Zigbee devices and colour or colour-temperature lights.** This is add-capability work, best done when such a device is actually in the house.
- **Built-in decoders for ESPHome and Tasmota.** Only worth it if devices get reflashed. Until then, plugins cover it.
- **Multus or macvlan networking for the HAP pod.** `hostNetwork` works on the Pi, so this only matters if node networking becomes a problem.

## Not doing

- **Home Assistant**, as a component, driver, integration source or dependency.
- **Runtime replicas or leader election.** A single replica is deliberate, because two would run every automation twice.
- **Homebridge as a driver.** It was rejected in [0002](specs/0002-plugin-drivers.md), because it makes Homebridge rather than Git the source of device identity.
- **Go `plugin` or WASM plugins.** The first needs CGO, and neither can do LAN multicast usefully.
- **Configuration from a UI.** Changes made in the zigbee2mqtt frontend count as drift, by design.

## Done

- **2026.42.1: Oikade core.** Device, Room, Automation, Bridge and Zigbee2MQTT. The zigbee2mqtt driver, the runtime (status, metrics, CEL automations), the HAP bridge, the Mosquitto and Grafana packaging, Kind e2e, and the Raspberry Pi guide (`3751951`…`d30535a`).
