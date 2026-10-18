# 0001: Room-targeted automations

- **Status:** Accepted
- **Roadmap:** unplanned
- **Updated:** 2026-10-18

<!-- design-feature fills in everything down to "Decision". write-spec fills in the rest.
     Delete any section that really doesn't apply, and say why in one line rather than leaving it empty. -->

## Problem

At 23:30 a "goodnight" automation should turn off every light in the living room. Today a `set` action names exactly one Device (`api/home/v1alpha1/automation_types.go`, `SetAction.Device`), so the automation lists each lamp by name. That is verbose. Worse, it goes stale without anyone noticing: add a third lamp to the living room by giving it `roomRef: living-room`, and it stays on at night until someone remembers to edit every automation that was meant to cover "the living room".

Triggers have the same problem. The hallway has two motion sensors, and "motion in the hallway" needs one trigger per sensor. A third sensor gets missed the same way the new lamp does.

The person who notices is someone standing in a dark hallway or a lit living room at midnight, not the person who wrote the Git change, and nothing in Grafana or `kubectl` tells them why.

Done looks like this:

- One action turns off every light in a room.
- One trigger fires on motion from any sensor in a room.
- A Device added to that room later is covered without editing the Automation.
- A misspelled room shows up as `Ready=False` on the Automation.

## Constraints

- **Plane.** Room membership is already config (`Device.spec.roomRef`), and the runtime already holds it in memory as `hub.Meta.Room` (`internal/hub/hub.go` `metaFor`). Resolving a room to its devices is a lookup against `hub.Store`. It adds nothing to etcd or MQTT.
- **Public API.** This adds `room` to `SetAction` and to `Trigger`, and relaxes `device` on both from required to "exactly one of". Existing YAML stays valid. Capability names, HomeKit IIDs and the CEL variables don't change. `trigger.device` keeps meaning the actual device that changed. `oikade_automation_runs_total{namespace,automation,result}` keeps its labels and result values. Adding a `room` label there would change series identity, so it isn't added.
- **State that must survive.** None is touched.
- **Component boundaries.**
  - The runtime engine changes: `internal/engine/compile.go` (`Compile`, `matches`) and `engine.go` (`handle`, `execute`).
  - The manager's `AutomationReconciler` adds the Room existence check. It has to watch Rooms so that creating a missing Room flips the Automation back to `Ready=True`.
  - hap is unaffected.
  - The runtime doesn't check whether a Room exists. A room that doesn't exist simply matches no devices.
- **Resolve at run time, not compile time.** `Compile` already leaves device existence to run time. A room's members also change without the Automation's generation changing, and the loader only reloads on a generation change (`internal/engine/controllers.go`). So actions read membership from `hub.Store` on each run, and triggers read the changed device's room on each update.
- **Pi.** A few extra MQTT `/set` publishes per run, and one store lookup per update. Negligible.

## Options

### A. `room` on `set` and on triggers, filtered by capability

`SetAction` and `Trigger` each gain `room`, mutually exclusive with `device`.

- **Room actions:** the engine sets every Device in the namespace whose `roomRef` is that room and whose discovered capabilities (`driver.Info.Capabilities`) include the action's capability, in name order. The capability check is what makes `light.on: false` on a room skip the motion sensor and the thermostat instead of failing on them.
- **Room triggers:** a trigger matches a change from any Device currently in that room.

Pros and cons:

- Pros: smallest change. It uses a concept the user already declares and reviews. A reviewer reading the Automation diff can see "living-room" and know what it means.
- Cons: only rooms. "Everything downstairs" or "all the lamps but not the ceiling light" can't be expressed.
- Cost of reversing: low. It's an optional field on two structs. If selectors (B) arrive later, `room` stays as shorthand for "roomRef equals X".

### B. A label selector on `set` and on triggers

Both structs gain `selector` (a `metav1.LabelSelector`) that matches Device `metadata.labels`, filtered by capability as in A. The user labels Devices with `oikade.dev/floor: downstairs` or `oikade.dev/kind: lamp`. Rooms would be covered by a label the manager mirrors from `roomRef`, or by labelling twice.

- Pros: general. Floors, zones and "lamps only" all come for free, and it's idiomatic Kubernetes.
- Cons: the label vocabulary becomes a second, unvalidated API in the user's repo. A typo'd label silently matches nothing, and unlike a Room name there's no object to check it against. Room ends up with two sources of truth, `roomRef` and a label. The runtime also has to carry labels into `hub.Meta`.
- Cost of reversing: medium. Once automations and Devices across the repo share a label scheme, changing it means editing both together.

### C. Don't build it: keep listing devices

Keep one `set` and one trigger per device. For manual control, the Home app's room tile can already turn a whole room off.

- Pros: no code, and it's fully explicit.
- Cons: it leaves the stale-automation problem exactly as it is. The Home app's room grouping is also assigned by hand in the Home app (see `RoomSpec` doc comment), so it isn't GitOps and Oikade automations can't use it.
- Cost of reversing: none.

## Decision

**Accepted: A, for both actions and triggers.** It fixes the real failure, a new device silently left out, with one field and a concept already in Git. Because Room is an object, a typo can be caught, which B's labels can't offer. A doesn't block B: `selector` can be added later as a third, mutually exclusive target. What we give up for now is floors and arbitrary groups.

What the user would commit:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Automation
metadata:
  name: hallway-motion-light
spec:
  mode: Restart
  triggers:
  - room: hallway
    capability: sensor.occupancy-detected
  condition: trigger.value && (now.getHours(tz) >= 18 || now.getHours(tz) < 7)
  actions:
  - set: {room: hallway, capability: light.on, value: true}
  - delay: 5m
  - set: {room: hallway, capability: light.on, value: false}
```

Public names introduced: `spec.triggers[].room` and `spec.actions[].set.room`. Each one is a Room object name, a plain string like `device`. The new condition reason is `RoomNotFound`. There are no new metrics or CEL variables.

Agreed semantics:

- **Membership** comes from `hub.Store`. It's read when an action runs, and when an update arrives for a trigger. Devices that haven't been discovered yet are skipped by room actions, because their capabilities are unknown.
- **Missing Room:** the manager sets `Ready=False, reason=RoomNotFound` when any trigger or action names a Room that doesn't exist in the namespace. The runtime still loads the Automation, and the missing room matches nothing. That's consistent with how a missing device behaves today. The check covers rooms only. Checking device names is a separate change.
- **Empty match** (the Room exists, but no discovered device in it has the capability): the action is a no-op, logged at `V(1)` as `"Matched no Devices in Room"`. The run still counts as `succeeded`. A metric can be added later if the log turns out not to be enough. It would be additive, with no new `result` value.
- **Partial failure:** the engine tries every matched device and logs each failure. The run counts as `failed` if any device failed. A dead Zigbee bulb shouldn't leave the rest of the room on.
- **Restart mode with a room trigger:** every occupancy event from any sensor in the room restarts the run, which is the behaviour the hallway example wants. write-spec should check how zigbee2mqtt re-reports occupancy while someone stays in range of sensor A. If it doesn't re-report, the lights turn off while sensor A is still occupied.

## Non-goals

- **Room aggregates in CEL**, such as "is any light on in the living room", or a `trigger.room` key. Either would extend the CEL contract, so they need their own decision.
- **Checking that Devices exist** at the `Ready` level. That's a natural follow-up to `RoomNotFound`, but out of scope here.
- **Floors, zones and arbitrary groups.** That's option B, deferred.
- **Pushing rooms to HomeKit.** HAP can't do this.

---

## API

TODO (write-spec)

## Behaviour

TODO (write-spec)

## State and upgrade

TODO (write-spec)

## Observability

TODO (write-spec)

## Security

TODO (write-spec)

## Test plan

TODO (write-spec)

## Docs

TODO (write-spec)

## Implementation plan

TODO (write-spec)

## Open questions

1. **Does zigbee2mqtt re-report occupancy** while someone stays in front of one sensor? This decides whether Restart mode with a room trigger can switch the lights off too early. Answer: check on the Pi with a real sensor. Blocks: the Behaviour section's Restart-mode guidance, not the implementation.
