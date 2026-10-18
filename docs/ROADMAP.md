# Oikade roadmap

_Updated 2026-10-18._ How to change this: edit it in a PR. Specs live in [docs/specs](specs/). Work is tracked in Backlog.md, with one milestone per section below.

## Direction

Oikade runs the whole house from Git: every device declared, HomeKit through its own bridge, and history in Prometheus. Next it brings the non-Zigbee devices (the SONOFF relays and the pool pump) in through plugins, and adds schedules that can run the pool, so that Homebridge can be retired. It is not a general-purpose hub: it has no Home Assistant, no UI for configuration, and no cloud.

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
| Room-targeted automations: write-spec, then build | [0001](specs/0001-room-targeted-actions.md) | M | Occupancy check (guidance only) | Accepted, needs write-spec |

Room-targeted automations are here, not in Next, because they're small and improve automations that this milestone starts running for real. The occupancy check they wait on happens during bring-up anyway.

## Next: Plugin drivers

**Goal:** the SONOFF MINIR4 relays run through Oikade, and a new device family no longer needs a core change.

**Done when:**

- The porch and hall relays are Devices in Git, appear under the Oikade Bridge in the Home app and in Grafana as `switch.on` (or `light.on`), and an Automation switches the porch light.
- The Homebridge MINIR4 plugin is uninstalled.
- `make test` and CI run the plugin contract conformance suite.
- The SONOFF plugin pod stays under 128 MiB RSS on the Pi.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|
| Decide how this repo gets `sonoff-lan` | [0002](specs/0002-plugin-drivers.md), open question | S | — | Not started |
| Contract, driver-agnostic hub, `Plugin` API, runtime and HAP wiring (steps 1–4) | [0002](specs/0002-plugin-drivers.md) | L | Pi bring-up | Not started |
| Plugin reconciler in the manager, TypeScript SDK, conformance, Kind e2e (steps 5–7) | [0002](specs/0002-plugin-drivers.md) | M | Steps 1–4 | Not started |
| SONOFF MINIR4 plugin, Pi checks, docs (steps 8–9) | [0002](specs/0002-plugin-drivers.md) | M | Steps 5–7, `sonoff-lan` decision | Not started |

This comes after bring-up because the plugin contract and its Pi constraints (`hostNetwork`, multicast, Node memory) are cheaper to get right once Oikade is actually running there.

## After that: Pool on Oikade

**Goal:** the pool pump runs on an Oikade Schedule, and Homebridge is retired.

**Done when:**

- Each morning, `kubectl get schedule pool-filtration` shows a target matching the water-temperature band, with blocks in off-peak and swimming hours. Credited minutes reach the target by the end of the day, and Grafana shows both.
- Temporarily raising the frost rule's threshold turns the pump on within a minute.
- Toggling the pump in the Home app holds it for 2 hours, and the `Pool automatic` switch pauses the schedule.
- Homebridge is uninstalled.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|
| Schedule planner, API, validation, runner, live rules and overrides (steps 1–5) | [0003](specs/0003-schedules.md) | L | — | Accepted |
| `Pool automatic` switch in HomeKit (step 6) | [0003](specs/0003-schedules.md) | S | 0002 steps 1–4 | Accepted |
| Schedule docs, e2e and dashboard (step 7) | [0003](specs/0003-schedules.md) | S | Step 6 | Accepted |
| TH Elite plugin: relay and water temperature as capabilities | needs a short addition to [0002](specs/0002-plugin-drivers.md) | M | Plugin drivers | Not started |

This comes after plugin drivers, because the pool needs the TH Elite plugin for its relay and probe, and the HomeKit switch reuses the plugin contract. Steps 1–5 of spec 0003 don't depend on anything, though, and the planner is the riskiest piece. So they're a good choice for spare evenings during Now or Next, and can be moved earlier if plugin work stalls.

## Later

- **Cozytouch heater plugin.** It's the second plugin, and proves the contract with a cloud-backed family. It comes after the pool, which matters more in summer.
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
