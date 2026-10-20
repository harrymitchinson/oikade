# Roadmap

Where Oikade is going, the order of the milestones and why, and what it deliberately won't do. Issues and their status are on the [Oikade board](https://github.com/users/harrymitchinson/projects/5). Each milestone's goal and exit criteria are in its description on [GitHub](https://github.com/harrymitchinson/oikade/milestones). Open issues with no milestone are the "later" list.

## Direction

Oikade runs the whole house from Git: every device is declared there, HomeKit goes through Oikade's own bridge, and history lives in Prometheus.

- **Done:** a plugin system, so device platforms other than Zigbee can join without core changes. Each plugin targets a platform, not a device.
- **Next:** one rule model for timed, conditional and budgeted behaviour, and a first release.
- **Then:** running on real hardware, where HomeKit and real devices can be checked.

It isn't a general-purpose hub. It has no Home Assistant driver, no UI for configuration and no cloud.

## Milestones, in order

Each milestone is a release, named by its semver version. Everything merged before a release ships in it.

1. **[v0.1.0](https://github.com/harrymitchinson/oikade/milestone/1).** The unified rule model ([#60](https://github.com/harrymitchinson/oikade/issues/60)) replaces `Schedule` and renames `Automation`, so it comes first: everything after it writes YAML against that API. Budgets ([#19](https://github.com/harrymitchinson/oikade/issues/19)) build on it. Release automation ([#36](https://github.com/harrymitchinson/oikade/issues/36)) is here so the first tag is cut the way every later one will be.
2. **[v0.2.0](https://github.com/harrymitchinson/oikade/milestone/2).** Real hardware. HomeKit pairing and real devices can only be checked on the Pi, and every Pi-only check is in [#27](https://github.com/harrymitchinson/oikade/issues/27). The longer features pile up untested there, the more this milestone will turn up. The eWeLink LAN plugin ([#37](https://github.com/harrymitchinson/oikade/issues/37)) is the first plugin for real devices.
3. **[v0.3.0](https://github.com/harrymitchinson/oikade/milestone/3).** The first plugins for other platforms: Overkiz ([#69](https://github.com/harrymitchinson/oikade/issues/69)), generic HTTP and webhook devices ([#70](https://github.com/harrymitchinson/oikade/issues/70)) and TP-Link Kasa and Tapo ([#71](https://github.com/harrymitchinson/oikade/issues/71)). It comes after v0.2.0 because a plugin is only done once it's checked against real devices, and the eWeLink LAN plugin proves the Go SDK on real hardware first.

## Not doing

- **Home Assistant as a driver.** Standalone device libraries that Home Assistant also uses are fine inside plugins.
- **Runtime replicas or leader election.** A single replica is deliberate, because two would run every automation twice.
- **Homebridge as a driver.** It would make Homebridge, rather than Git, the source of device identity ([0002](design/0002-plugin-drivers.md)).
- **Go `plugin` or WASM plugins.** The first needs CGO, and neither can do LAN multicast usefully.
- **Configuration from a UI.** Changes made in the zigbee2mqtt frontend count as drift, by design.
