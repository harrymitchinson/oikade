---
id: TASK-0007
title: Hub hardening from the TASK-0001.04 review
status: To Do
assignee: []
created_date: '2026-10-18 14:30'
labels: []
dependencies:
  - TASK-0001.04
references:
  - internal/hub/hub.go
  - internal/runtime/runtime_test.go
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Follow-up findings from the code review of PR #9 (TASK-0001.04). None of them changes what users see today, but each costs work or risks a stall.

1. internal/hub/hub.go: ensure and stop hold h.mu while calling inst.Start (Bus.Subscribe waits for SUBACK, with a context that never expires) and inst.Stop (unsubscribe can wait up to 10s). Hub.Set, from automations and HomeKit, and Hub.PluginStats, on every /metrics scrape, block on that lock, so a slow or disconnected broker during a Plugin or Zigbee2MQTT create or delete stalls commands and scrapes. Move Start and Stop outside the lock, for example by reserving the map slot and then subscribing, or have PluginStats read a snapshot that doesn't need h.mu.
2. Both hub controllers lack predicates. Every Device status patch from status.Writer maps back to the driver object, so the hub re-lists the namespace's Devices and SetDevices re-emits Discovered and replays state for every device, which in turn enqueues a status writer reconcile for each one. Add predicates such as GenerationChangedPredicate on For and the Device watch, keeping create and delete events.
3. In reconcilePlugin, the current check compares pi.Root with p.Topic(), which is derived from namespace and name and so can't change for a key. The type assertions in both current funcs can't fail either, because the key carries the driver. Simplify them.
4. internal/runtime/runtime_test.go startRuntime registers t.Cleanup(cancel) only at the end, so a t.Fatal earlier in setup leaves the 60s context and the MQTT client running. Register cancel early, but keep the runtime stopping before the broker and the API server.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Hub.Set and Hub.PluginStats don't wait on broker round trips; a test with a Bus whose Subscribe blocks shows Set and PluginStats still return
- [ ] #2 A Device status patch no longer triggers a hub reconcile; spec changes, creates and deletes still do
- [ ] #3 The hub's current checks contain no branches that can't be taken
- [ ] #4 startRuntime cancels its context on every exit path, and the runtime still stops before the broker and API server
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 Tests pass
- [ ] #2 Documentation updated
- [ ] #3 No regressions introduced
<!-- DOD:END -->
