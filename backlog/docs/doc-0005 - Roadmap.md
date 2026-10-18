---
id: doc-0005
title: Roadmap
type: readme
created_date: '2026-10-18 10:58'
updated_date: '2026-10-18 10:58'
---
This doc holds what Backlog can't: where Oikade is going, the order of the milestones and why, what each one must prove, and what we've decided not to do. Everything with a status lives in Backlog: `backlog milestone list`, `backlog task list -m "<milestone>"`, and `backlog draft list` for ideas that aren't ready to build. Drafts with no milestone are the "later" list.

## Direction

Oikade runs the whole house from Git: every device declared, HomeKit through its own bridge, and history in Prometheus. Next it gains a plugin system, so that non-Zigbee device families (the SONOFF relays, the pool pump, the Cozytouch heaters) can join without core changes, and schedules that can run the pool. Then Homebridge can be retired. It is not a general-purpose hub: it has no Home Assistant, no UI for configuration, and no cloud.

## Milestones, in order

### 1. Plugin drivers

**Goal:** a new device family no longer needs a core change, and automations can target whole rooms.

**Done when:**

- CI runs the plugin contract conformance suite against the simulated example plugin.
- In Kind, a `Plugin` and a `Device` declared in Git and served by the example plugin reach `Resolved=True`. They appear in `oikade_capability_value`, and an Automation switches them.
- Writing a new plugin needs no change to `internal/hub` or `api/home`.
- In Kind, one `set` action with `room:` turns off both lamps in a Room, and a misspelled room shows `Ready=False RoomNotFound`.

**Why first:** the plugin contract and the `Device` source rule are public API that Schedules builds on, and they're cheapest to settle before more YAML depends on them. Room-targeted automations rode along because they were small and independent.

### 2. Schedules

**Goal:** anything with an on/off capability can run on a schedule, from fixed windows to a daily budget shaped by sensors.

**Done when:**

- In Kind, a Schedule with a single `require` window drives a faked Zigbee plug on and off at the window edges.
- A budgeted Schedule writes `status.today` at planning time with the expected blocks. Credited `runMinutes` follow the plug's reported state.
- A live `require` rule turns the target on within a minute of its condition, a manual toggle holds for `override.duration`, and the control Device's `/set` pauses the schedule.
- Grafana's "Schedules" row shows target against credited minutes.

**Why here:** it needs the plugin contract for its control Device. The planner has no dependencies and is the riskiest piece, so it's a good spare-evening task during Plugin drivers.

### 3. Retire Homebridge

**Goal:** the SONOFF relays and the pool run on Oikade on the Pi, and Homebridge is uninstalled.

**Done when:**

- Every Pi bring-up check passes.
- The hall and porch MINIR4 relays are Devices in Git, toggled from the Home app through the Oikade Bridge.
- Each morning, `kubectl get schedule pool-filtration` shows a target matching the water-temperature band, and credited minutes reach it by the end of the day.
- Homebridge is uninstalled.

**Why here:** it needs both plugins and schedules. Pi bring-up is parked by choice but is a hard prerequisite, because HomeKit pairing and real devices can only be checked on the Pi. The longer features pile up untested on real hardware, the more this milestone will turn up, so resume bring-up as soon as Plugin drivers is done.

## Not doing

- **Home Assistant**, as a component, driver, integration source or dependency.
- **Runtime replicas or leader election.** A single replica is deliberate, because two would run every automation twice.
- **Homebridge as a driver.** Rejected in doc-0002, because it makes Homebridge rather than Git the source of device identity.
- **Go `plugin` or WASM plugins.** The first needs CGO, and neither can do LAN multicast usefully.
- **Configuration from a UI.** Changes made in the zigbee2mqtt frontend count as drift, by design.
