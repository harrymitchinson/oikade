---
id: TASK-0005
title: Check whether zigbee2mqtt re-reports occupancy
status: To Do
assignee: []
created_date: '2026-10-18 10:57'
labels: []
milestone: m-5
dependencies:
  - TASK-0004
documentation:
  - doc-0001
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The README's recommended Restart-mode pattern for room motion automations assumes zigbee2mqtt re-reports occupancy while someone stays in front of one sensor. That is only observable on real hardware (open question 1 in doc-0001).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 With someone standing in front of one occupancy sensor, it is recorded whether zigbee2mqtt publishes repeated occupancy=true messages and how often
- [ ] #2 The README's Restart-mode guidance matches the observed behaviour
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
