---
id: TASK-0002
title: Room-targeted automations
status: To Do
assignee: []
created_date: '2026-10-18 08:19'
updated_date: '2026-10-18 09:06'
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
- [ ] #1 One set action switches every Device in a room that has the capability
- [ ] #2 One trigger fires on a change from any Device in a room
- [ ] #3 A Device added to the room later is covered without editing the Automation
- [ ] #4 A misspelled room shows as Ready=False on the Automation
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
