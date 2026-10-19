# Roadmap

Where Oikade is going, the order of the milestones and why, and what it deliberately won't do. Issues and their status are on the [Oikade board](https://github.com/users/harrymitchinson/projects/5). Each milestone's goal and exit criteria are in its description on [GitHub](https://github.com/harrymitchinson/oikade/milestones). Open issues with no milestone are the "later" list.

## Direction

Oikade runs the whole house from Git: every device is declared there, HomeKit goes through Oikade's own bridge, and history lives in Prometheus.

- **Done:** a plugin system, so device families other than Zigbee can join without core changes.
- **Next:** schedules that can run the pool.
- **Then:** Homebridge can be retired.

It isn't a general-purpose hub. It has no Home Assistant driver, no UI for configuration and no cloud.

## Milestones, in order

1. **Plugin drivers** (done). The plugin contract and the `Device` source rule are public API that Schedules builds on, so they had to settle before more YAML depended on them.
2. **[Schedules](https://github.com/harrymitchinson/oikade/milestone/1).** It needs the plugin contract for its control Device. The planner has no dependencies and is the riskiest piece, so build it first. Its design ([#19](https://github.com/harrymitchinson/oikade/issues/19), `docs/design/0003-schedules.md`) needs your review before it's built.
3. **[Retire Homebridge](https://github.com/harrymitchinson/oikade/milestone/2).** It needs both plugins and schedules. Pi bring-up is a hard prerequisite, because HomeKit pairing and real devices can only be checked on the Pi. The longer features pile up untested on real hardware, the more this milestone will turn up.

The Go plugin SDK ([#30](https://github.com/harrymitchinson/oikade/issues/30)) isn't in a milestone yet.

## Not doing

- **Home Assistant as a driver.** Standalone device libraries that Home Assistant also uses are fine inside plugins.
- **Runtime replicas or leader election.** A single replica is deliberate, because two would run every automation twice.
- **Homebridge as a driver.** It would make Homebridge, rather than Git, the source of device identity ([0002](design/0002-plugin-drivers.md)).
- **Go `plugin` or WASM plugins.** The first needs CGO, and neither can do LAN multicast usefully.
- **Configuration from a UI.** Changes made in the zigbee2mqtt frontend count as drift, by design.
