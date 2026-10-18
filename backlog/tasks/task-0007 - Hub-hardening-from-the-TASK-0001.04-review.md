---
id: TASK-0007
title: Hub hardening from the TASK-0001.04 review
status: Done
assignee:
  - '@claude'
created_date: '2026-10-18 14:30'
updated_date: '2026-10-18 15:05'
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
- [x] #1 Hub.Set and Hub.PluginStats don't wait on broker round trips; a test with a Bus whose Subscribe blocks shows Set and PluginStats still return
- [x] #2 A Device status patch no longer triggers a hub reconcile; spec changes, creates and deletes still do
- [x] #3 The hub's current checks contain no branches that can't be taken
- [x] #4 startRuntime cancels its context on every exit path, and the runtime still stops before the broker and API server
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 Tests pass
- [x] #2 Documentation updated
- [x] #3 No regressions introduced
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. hub.ensure/stop: reserve or clear the map slot under h.mu, run Start/Stop outside it; PluginStats snapshots instances then reads Stats unlocked
2. GenerationChangedPredicate on For and the Device watch of both hub controllers
3. Drop unreachable branches in the current checks (plugin root follows from the key; key carries the driver)
4. startRuntime: create ctx after the broker and API server, register cancel at once
5. Tests: hub unit test with a gated Bus; runtime test counting hub-plugin reconciles around a status patch
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Instance interface gains Start so ensure owns the start/log. Per-key reconciles never overlap, so the reserved slot is safe without the lock.
TestBrokerWaitsDontBlockSet (internal/hub) and TestHubIgnoresStatusPatches (internal/runtime) both fail against the old hub.go and pass now.
Local toolchain: tests need CGO_ENABLED=0 (SDK tbd linker error) and lint needs GOTOOLCHAIN=go1.26.0; both unrelated to this change. TestReloadingTheSameGenerationKeepsTheRun flaked once in make test, passed 5/5 on rerun.
No user-facing docs describe hub locking or watches; comments updated in hub.go.

Code review follow-ups: ensure takes declared and calls SetDevices on a new instance before it enters the map (otherwise Set saw 'not declared' during Start); Start is bounded to 10s because Client.Subscribe registers the handler before SUBACK and reconnects resubscribe. Kept the unchecked type assertion: AC #3 asks for it and the key carries the driver. Kept the 2s quiet window in TestHubIgnoresStatusPatches; it fails reliably against the old code.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The hub no longer holds h.mu across broker round trips. ensure reserves the map slot, gives a new instance its devices, then stops the old instance and starts the new one outside the lock, with Start bounded to 10s. stop removes the slot and then stops the instance. PluginStats snapshots the instances and reads Stats unlocked. Both hub controllers use GenerationChangedPredicate on For and the Device watch, so status writes no longer re-list Devices and replay state. The current checks lost their unreachable branches: the plugin check is gone, because the root follows from the key, and the Zigbee2MQTT assertion is unchecked. startRuntime creates its context after the broker and API server and registers cancel at once.

Verification: TestBrokerWaitsDontBlockSet (gated Bus, covering subscribe and unsubscribe) and TestHubIgnoresStatusPatches (counts hub-plugin reconciles around a status patch, a spec change and a delete) both fail against the old hub.go and pass now. The full internal suite passes and golangci-lint reports 0 issues. Local quirks unrelated to this change: tests need CGO_ENABLED=0 and lint needs GOTOOLCHAIN=go1.26.0. TestReloadingTheSameGenerationKeepsTheRun flaked once and then passed on rerun.

Risk: during a Zigbee2MQTT baseTopic change, Set reaches the new instance before it has subscribed and seen bridge/devices, so commands fail fast with 'not found' instead of waiting.
<!-- SECTION:FINAL_SUMMARY:END -->
