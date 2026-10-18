---
id: TASK-0004
title: Pi bring-up
status: To Do
assignee: []
created_date: '2026-10-18 10:57'
labels: []
milestone: m-5
dependencies: []
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Nothing has run outside Kind yet, and HomeKit pairing and real Zigbee devices can only be checked on the Pi cluster. Parked by choice; resume as soon as Plugin drivers is done, because every feature built before then is untested on real hardware. Follow docs/deploy-raspberry-pi.md. Also run the manual Pi checks recorded in doc-0001 and doc-0002.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Oikade is deployed to the Pi cluster by GitOps
- [ ] #2 Every declared Zigbee device shows Resolved=True and Available=True
- [ ] #3 The Bridge is paired and the Home app works with the runtime scaled to zero
- [ ] #4 A namespace delete followed by a restore re-pairs nothing
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
