---
id: DRAFT-0002
title: SONOFF MINIR4 plugin
status: Draft
assignee: []
created_date: '2026-10-18 10:57'
updated_date: '2026-10-18 10:57'
labels: []
milestone: m-5
dependencies: []
documentation:
  - doc-0002
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The hall and porch MINIR4 relays are the first real devices to move off Homebridge onto Oikade. Needs design-feature first: where the plugin lives and how it gets the sonoff-lan package are undecided (doc-0002 non-goals). Earlier acceptance ideas: each MINIR4 reports switch.on (or parameters.capability); a missing LAN key or bad parameter is a per-device error; on the Pi, wall-switch changes reach the metric within 5s, Home app toggles are confirmed within 10s, and toggles work with the runtime scaled to zero.
<!-- SECTION:DESCRIPTION:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
