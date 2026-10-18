---
id: TASK-0001
title: Plugin drivers
status: Done
assignee:
  - '@claude'
created_date: '2026-10-18 08:05'
updated_date: '2026-10-18 17:42'
labels: []
milestone: m-3
dependencies: []
documentation:
  - doc-0002
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Every non-Zigbee device family (SONOFF LAN relays, Cozytouch heaters) needs an Oikade core change today, so these devices live in a separate Homebridge, outside Git, Grafana and automations. Design: doc-0002
<!-- SECTION:DESCRIPTION:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 Tests pass
- [x] #2 Documentation updated
- [x] #3 No regressions introduced
<!-- DOD:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The plugin system from doc-0002 is built: the oikade/v1 contract and decoder (.01), the driver-agnostic hub (.02), the Plugin kind and Device.spec.plugin (.03), runtime and HAP wiring (.04), the manager's PluginReconciler (.05), the TypeScript SDK, example-switch plugin, conformance suite and multi-arch images (.06), Kind e2e coverage including /set (.07), and docs (.09). The SONOFF MINIR4 plugin (.08) was dropped from this spec and archived; real device plugins are separate work. PRs #5, #7, #8, #9, #11, #12, #14 and this one.
<!-- SECTION:FINAL_SUMMARY:END -->
