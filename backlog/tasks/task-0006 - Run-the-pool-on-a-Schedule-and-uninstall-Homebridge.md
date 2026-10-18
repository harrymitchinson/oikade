---
id: TASK-0006
title: Run the pool on a Schedule and uninstall Homebridge
status: To Do
assignee: []
created_date: '2026-10-18 10:57'
labels: []
milestone: m-5
dependencies:
  - TASK-0003
  - TASK-0004
documentation:
  - doc-0003
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Homebridge is the last thing running outside Git. Once the pool pump runs on Oikade, it can go. Also needs the TH Elite plugin, which is still a draft in this milestone (promote it and add it as a dependency when it becomes a task).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Each morning `kubectl get schedule pool-filtration` shows a target matching the water-temperature band
- [ ] #2 Credited minutes reach the target by the end of the day
- [ ] #3 The hall and porch MINIR4 relays are toggled from the Home app through the Oikade Bridge
- [ ] #4 Homebridge is uninstalled
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
