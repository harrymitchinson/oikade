---
id: TASK-0002
title: Room-targeted automations
status: Done
assignee:
  - '@claude'
created_date: '2026-10-18 08:19'
updated_date: '2026-10-18 09:31'
labels:
  - spec-0001
milestone: m-1
dependencies: []
documentation:
  - docs/specs/0001-room-targeted-actions.md
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A set action or trigger can only name one Device, so automations meant for "the living room" silently miss devices added to that room later. Spec 0001 (Accepted) decides to add room to set actions and triggers; its implementation half still needs write-spec.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 One set action switches every Device in a room that has the capability
- [x] #2 One trigger fires on a change from any Device in a room
- [x] #3 A Device added to the room later is covered without editing the Automation
- [x] #4 A misspelled room shows as Ready=False on the Automation
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 Tests pass
- [x] #2 Documentation updated
- [x] #3 No regressions introduced
<!-- DOD:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Evidence for each criterion, from the subtasks:
- AC1: TestRoomActionSetsMembers, TestRuntimeEndToEnd (both lamps, not the plug) and the Kind e2e (both lamps).
- AC2: TestRoomTriggerMatches, plus the porch room trigger in TestRuntimeEndToEnd and the Kind e2e.
- AC3: membership is read from hub.Store on each update and each action, with no per-Automation cache; TestRoomMembershipAtExecution adds a lamp mid-run.
- AC4: RoomNotFound envtest and the Kind e2e.
Still open, parked with Pi bring-up: the spec's manual Pi checks, including whether zigbee2mqtt re-reports occupancy, which decides the final README Restart-mode recommendation.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Spec 0001 is implemented in four commits on feat/room-targeted-automations: the room API and Compile, engine room resolution at run time, manager RoomNotFound with a Room watch, and docs, sample and Kind e2e. Unit, integration, envtest and Kind e2e tests all pass. The Pi manual checks stay parked with Pi bring-up.
<!-- SECTION:FINAL_SUMMARY:END -->
