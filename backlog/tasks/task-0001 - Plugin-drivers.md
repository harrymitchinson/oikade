---
id: TASK-0001
title: Plugin drivers
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-18 08:05'
updated_date: '2026-10-18 10:57'
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
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
