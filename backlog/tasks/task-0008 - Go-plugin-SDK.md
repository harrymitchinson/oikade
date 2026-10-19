---
id: TASK-0008
title: Go plugin SDK
status: To Do
assignee: []
created_date: '2026-10-18 18:26'
updated_date: '2026-10-19 07:54'
labels: []
dependencies: []
documentation:
  - doc-0006
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Go is the first plugin language, but the only SDK is handwritten TypeScript, and Oikade's decoder and any Go SDK would each carry their own payload types. Add shared contract types (contract/v1) used by internal/driver/plugin, a Go SDK (sdk/plugin), rewrite example-switch in Go, and remove the TypeScript workspace and Node CI. A derived JSON Schema and a standalone conformance runner wait for the first non-Go plugin. Design: doc-0006
<!-- SECTION:DESCRIPTION:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
