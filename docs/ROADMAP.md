# Oikade roadmap

_Updated 2026-10-18._ How to change this: edit it in a PR. Specs live in [docs/specs](specs/). Work is tracked in Backlog.md, with one milestone per section below.

## Direction

Oikade runs the whole house from Git: every device declared, HomeKit through its own bridge, and history in Prometheus. Next it gains a plugin system, so that non-Zigbee device families (the SONOFF relays, the pool pump, the Cozytouch heaters) can join without core changes, and schedules that can run the pool. Then Homebridge can be retired. It is not a general-purpose hub: it has no Home Assistant, no UI for configuration, and no cloud.

## Now: Plugin drivers

**Goal:** a new device family no longer needs a core change, and automations can target whole rooms.

**Done when:**

- CI runs the plugin contract conformance suite against the simulated example plugin.
- In Kind, a `Plugin` and a `Device` declared in Git and served by the example plugin reach `Resolved=True`. They appear in `oikade_capability_value`, and an Automation switches them.
- Writing a new plugin needs no change to `internal/hub` or `api/home`.
- In Kind, one `set` action with `room:` turns off both lamps in a Room, and a misspelled room shows `Ready=False RoomNotFound`.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|
| Room-targeted automations (TASK-0002, steps 1–4) | [0001](specs/0001-room-targeted-actions.md) | M | — | Ready |
| Contract and decoder, driver-agnostic hub (steps 1–2) | [0002](specs/0002-plugin-drivers.md) | M | — | Ready |
| `Plugin` API, runtime and HAP wiring (steps 3–4) | [0002](specs/0002-plugin-drivers.md) | M | Steps 1–2 | Ready |
| Plugin reconciler, TypeScript SDK and conformance, Kind e2e (steps 5–7) | [0002](specs/0002-plugin-drivers.md) | M | Steps 3–4 | Ready |
| Plugin system docs (step 8) | [0002](specs/0002-plugin-drivers.md) | S | Steps 5–7 | Ready |

Plugin drivers come before schedules for two reasons. Their contract and `Device` source rule are public API that spec 0003 builds on, and those are cheapest to settle before more YAML depends on them. Room-targeted automations ride along because they're small, ready and independent. Start with either, but room automations are the quicker win.

The manual Pi checks in specs 0001 and 0002 wait for Pi bring-up (see Retire Homebridge).

## Next: Schedules

**Goal:** anything with an on/off capability can run on a schedule, from fixed windows to a daily budget shaped by sensors.

**Done when:**

- In Kind, a Schedule with a single `require` window drives a faked Zigbee plug on and off at the window edges.
- A budgeted Schedule writes `status.today` at planning time with the expected blocks. Credited `runMinutes` follow the plug's reported state.
- A live `require` rule turns the target on within a minute of its condition, a manual toggle holds for `override.duration`, and the control Device's `/set` pauses the schedule.
- Grafana's "Schedules" row shows target against credited minutes.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|
| Planner (step 1) | [0003](specs/0003-schedules.md) | M | — | Ready |
| API and manager validation (steps 2–3) | [0003](specs/0003-schedules.md) | M | Step 1; 0002 step 3 for the `Device` source rule | Ready |
| Runner, live rules and overrides (steps 4–5) | [0003](specs/0003-schedules.md) | L | Steps 2–3 | Ready |
| Control Device in HomeKit (step 6) | [0003](specs/0003-schedules.md) | S | Steps 4–5; 0002 steps 1–4 | Ready |
| Docs, e2e and dashboard (step 7) | [0003](specs/0003-schedules.md) | S | Step 6 | Ready |

The planner has no dependencies and is the riskiest piece, so it's a good spare-evening task during Now.

## After that: Retire Homebridge

**Goal:** the SONOFF relays and the pool run on Oikade on the Pi, and Homebridge is uninstalled.

**Done when:**

- Every Pi bring-up check passes:
  - Zigbee devices are `Resolved=True` and `Available=True` on the Pi, deployed by GitOps.
  - The Bridge is paired, and the Home app works with the runtime scaled to zero.
  - A namespace delete plus restore re-pairs nothing.
- The hall and porch MINIR4 relays are Devices in Git, toggled from the Home app through the Oikade Bridge.
- Each morning, `kubectl get schedule pool-filtration` shows a target matching the water-temperature band, and credited minutes reach it by the end of the day.
- Homebridge is uninstalled.

| Item | Spec | Size | Depends on | Status |
|---|---|---|---|---|
| Pi bring-up: rebuild and deploy by GitOps, pair the Bridge, declare the Zigbee devices, restore drill | — (ops, per [Pi guide](deploy-raspberry-pi.md)) | M | — | Parked |
| Check whether zigbee2mqtt re-reports occupancy | [0001](specs/0001-room-targeted-actions.md), open question | S | Pi bring-up | Parked |
| SONOFF MINIR4 plugin | needs design | M | Plugin drivers | Not started |
| TH Elite plugin: relay and water temperature as capabilities | needs design | M | Plugin drivers | Not started |
| Pool Schedule on the real pump, then remove Homebridge | [0003](specs/0003-schedules.md) | S | Schedules, TH Elite plugin, Pi bring-up | Not started |

Pi bring-up is parked by choice, but it's a hard prerequisite here, because HomeKit pairing and real devices can only be checked on the Pi. The longer features pile up untested on real hardware, the more this milestone will turn up. So resume bring-up as soon as Plugin drivers is done.

## Later

- **Time triggers on Automations** (`at`, `cron`, sunset). One-off actions such as "porch light at sunset". Spec 0003 left this out of scope; it needs design-feature.
- **Cozytouch heater plugin.** It proves the contract with a cloud-backed family. It comes after Retire Homebridge, which removes a running dependency.
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

- **Oikade core**, first tagged `2026.42.1` under the old date scheme. That tag was deleted when versioning moved to semver, and `v0.1.0` will be the first semver release. Device, Room, Automation, Bridge and Zigbee2MQTT. The zigbee2mqtt driver, the runtime (status, metrics, CEL automations), the HAP bridge, the Mosquitto and Grafana packaging, Kind e2e, and the Raspberry Pi guide (`3751951`…`d30535a`).
