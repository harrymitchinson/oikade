# 0001: Room-targeted automations

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
  - The runtime doesn't check whether a Room exists. It matches Devices by the name in their `roomRef`, so a misspelled room matches none, and a deleted Room still matches Devices whose `roomRef` names it.
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
- **Missing Room:** the manager sets `Ready=False, reason=RoomNotFound` when any trigger or action names a Room that doesn't exist in the namespace. The runtime still loads the Automation, and the room matches the Devices whose `roomRef` names it, which for a typo is none. That's consistent with how a missing device behaves today. The check covers rooms only. Checking device names is a separate change.
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

These changes go in `api/home/v1alpha1/automation_types.go`. `device` becomes optional on both structs, and a CEL rule requires exactly one of `device` or `room`:

```go
// Trigger fires the Automation when a capability of a device, or of any device in a room, changes value.
// +kubebuilder:validation:XValidation:rule="has(self.device) != has(self.room)",message="exactly one of device or room must be specified"
type Trigger struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Device string `json:"device,omitempty"`

	// room names a Room in the same namespace. The trigger matches a change from any Device whose
	// roomRef is this room at the time of the change.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Room string `json:"room,omitempty"`

	// capability is unchanged.
	Capability string `json:"capability"`
}

// SetAction sets a capability on a device, or on every device in a room that has the capability.
// +kubebuilder:validation:XValidation:rule="has(self.device) != has(self.room)",message="exactly one of device or room must be specified"
type SetAction struct {
	// device names a Device in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Device string `json:"device,omitempty"`

	// room names a Room in the same namespace. Every discovered Device in it that reports the capability is set,
	// in name order.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Room string `json:"room,omitempty"`

	// capability and value are unchanged.
	Capability string               `json:"capability"`
	Value      apiextensionsv1.JSON `json:"value"`
}
```

There are no new printer columns. The `Ready` condition gains the reason `RoomNotFound`. `Invalid` still takes precedence, because a spec that doesn't compile is reported first. The YAML example is in the Decision section above.

## Behaviour

### Manager: `AutomationReconciler` (`internal/controller/home/automation_controller.go`)

1. Run `engine.Compile` as today. On error, set `Ready=False Invalid`.
2. Otherwise, collect the distinct `room` values from the triggers and set actions. List the Rooms in the namespace. If any are missing, set `Ready=False RoomNotFound`, with the message `Room(s) not found: kitchen, livingroom`, sorted.
3. Otherwise, set `Ready=True Valid`.
4. Add `Watches(&homev1alpha1.Room{}, …)`, mapping a Room to every Automation in its namespace. That way, creating or deleting a Room flips `Ready` without the Automation being edited. Automations are few, so mapping to all of them in the namespace is cheaper than indexing.

### Runtime: the engine (`internal/engine`)

1. **`Compile`** (`compile.go`): `action` gains `room string`. As a defence in depth, if neither `device` nor `room` is set, Compile returns `triggers[i]: one of device or room is required` (and the same for `actions[i]`). The API rejects this already. Compile doesn't check whether a room exists, just as it doesn't for devices.
2. **A new store method** (`internal/hub/store.go`): `func (s *Store) Room(namespace, room string) []types.NamespacedName` returns the declared Devices whose `Meta.Room` matches, sorted by name. HAP doesn't use it.
3. **Matching triggers** (`engine.go` `handle`): before the loop, look up the changed device's current room once with `e.Store.Get(u.Device)`. `matches(device, room, capability)` is true for a trigger whose `Device == device`, or whose `Room` is set and equals `room`, when the capability also matches. A device with no room never matches a room trigger. `trigger.device` in CEL is still the device that actually changed.
4. **Running room actions** (`execute`): when `a.room` is set, resolve the members **at the moment the action runs**, not when the run started. A lamp added to the room during the `delay: 5m` is turned off with the others.
   - Skip members where `!snap.Discovered`, or where `a.capability` isn't in `snap.Info.Capabilities`.
   - Call `e.Setter.Set` on each remaining member in name order. Log each failure as `"Failed to run Automation action"`, with `device`, `room` and `capability`.
   - If nothing matched, log `"Matched no Devices in Room"` at `V(1)` and carry on.
5. **Results.** **A room action that fails on some devices doesn't stop the run.** The rest of the actions still run, and the run counts as `failed` at the end. This spells out the agreed rule, "a dead bulb shouldn't leave the rest of the room on". If the run stopped, `on, delay, off` would leave the working bulbs on forever. A failing **device** action still stops the run immediately, as it does today.

### Restart mode with a room trigger

Every matching change from any sensor in the room restarts the run. That's what the hallway example wants when people move between sensors. Whether one person *standing still* in front of a single sensor keeps the lights on depends on the parked open question below:

- Drivers only report real changes (`driver.Update` carries `Previous`). So a re-reported `occupancy: true` doesn't restart the run, whether or not zigbee2mqtt sends it. Someone standing still therefore produces no new trigger. The lights stay on only while the run's `delay` is longer than the time before the sensor next flips `false` and then `true` again.
- **The open question decides the guidance.**
  - If the sensor's `true` lasts for as long as someone is in range, the safe pattern is a delay longer than the sensor's occupancy timeout.
  - If it pulses `true`/`false` while someone is present, the delay only needs to cover the gap between pulses.
- **Switching off "when the room is empty"** would need room aggregates in CEL (is any sensor in this room occupied?). That's a non-goal here. Until it exists, the delay-based pattern is the only room-scoped one. The README will state the recommended delay once the Pi check is done, and the sample keeps today's delay-based form with a comment.

### Failure modes

| Situation | What happens | What the user sees |
|---|---|---|
| A Room is missing | The manager sets `RoomNotFound`. The runtime loads the Automation anyway. The room matches Devices whose `roomRef` names it: none for a typo, and the old members after a Room is deleted. | `kubectl get automations` shows `Ready=False`, with the room names. |
| The Room is created later | The Room watch re-reconciles. | `Ready=True`, with no edit needed. |
| A Device moves rooms | Membership is read from `hub.Store` on each update and each action. | The next trigger or action uses the new room. |
| A Device isn't discovered yet, or lacks the capability | It's skipped by room actions. | Nothing, apart from a `V(1)` log if no device matched. |
| A Device is offline | `Set` publishes as it does for device actions today. The driver doesn't refuse. | Same as today's device actions. |
| One bulb fails | The other bulbs are set and later actions run. | `oikade_automation_runs_total{result="failed"}`, and a log line naming the bulb. |
| Two sensors in a room fire together | Two updates arrive. Restart mode restarts twice, which is harmless. Single mode counts the second as `busy`. | As today. |
| The runtime restarts mid-run | The run is lost, as it is today. Room membership has no extra state. | As today. |
| The broker is down | `Set` fails for each member, and the run counts as `failed`. | As today, multiplied by the number of members. |

hap is unaffected.

## State and upgrade

- **Nothing new persists.** Membership comes from `Device.spec.roomRef`, held in memory by `hub.Store`.
- **Upgrade:** additive. Existing Automations have `device` set and pass the new CEL rule. Making `device` `omitempty` doesn't change how existing objects serialise.
- **Delete:** a deleted Automation is removed by the loader as it is today. A deleted Room makes Automations that reference it go to `RoomNotFound`. Their room triggers and actions still match Devices whose `roomRef` still names it.
- **Restoring from Git:** everything is config. Nothing needs a backup.

## Observability

- **No new metrics.** `oikade_automation_runs_total{namespace,automation,result}` keeps its labels and result values, as the top half decided.
- **Log messages:**
  - `"Matched no Devices in Room", "automation", key, "room", r, "capability", c` at `V(1)`
  - `"Failed to run Automation action"` gains a `room` key for room actions
- **Condition:** `Ready` reason `RoomNotFound`.
- **Grafana:** no change.

## Security

- **Manager RBAC:** add `// +kubebuilder:rbac:groups=home.oikade.dev,resources=rooms,verbs=get;list;watch` to `AutomationReconciler`. The manager has no access to Rooms today. Then run `make manifests`.
- The runtime and hap need no changes. Nothing new is exposed on the network, and no Secrets are involved.

## Test plan

| Behaviour | Layer | Test |
|---|---|---|
| Compile accepts room triggers and actions, and rejects neither (defence in depth) | Unit | `internal/engine` `TestCompileRoom` |
| A room trigger matches a member's change, but not a non-member's or a roomless device's | Unit | `TestRoomTriggerMatches`, with a `hub.Store` fixture |
| A room action sets discovered members with the capability, in name order, and skips the rest | Unit | `TestRoomActionSetsMembers`, with a fake `Setter` |
| An empty match succeeds and logs | Unit | `TestRoomActionEmptyMatch` |
| A partial failure continues the run and counts `failed` | Unit | `TestRoomActionPartialFailure` |
| Membership is resolved when the action runs (a device added during the delay is included) | Unit | `TestRoomMembershipAtExecution`, with a fake clock |
| `Store.Room` returns sorted members for that namespace only | Unit | `internal/hub` `TestStoreRoom` |
| Room action → `/set` on the bus for every lamp | Integration | `internal/runtime` with `mqtttest`: two z2m lamps in one room, one action, two `/set` publishes |
| CEL: exactly one of device or room, on triggers and set | envtest | `internal/controller/home` creates valid and invalid Automations |
| `RoomNotFound`, then `Ready=True` after creating the Room; `Invalid` takes precedence | envtest | `automation_controller_test.go` |
| A deployed room automation fires | Kind e2e | `test/e2e/home_test.go`: two fake lamps in a Room, a room action, and both `zigbee2mqtt/<ieee>/set` publishes seen |

Manual checks on the Pi (parked with the rest of Pi bring-up):

1. **Goodnight.** A room action at a fixed time turns off every light in the living room. Pass: every lamp is off within 2 s, and the run counts as `succeeded`.
2. **Two hallway sensors.** Walking past either sensor turns the hallway lights on. Pass: one `/set` per lamp within 1 s of the motion.
3. **The open question.** Stand still in front of one sensor for longer than the delay. Watch `oikade_capability_changes_total{capability="sensor.occupancy-detected"}` and the lights. Record whether the lights go off while you're still there. That decides which pattern the README recommends.

## Docs

- **README "Automations":**
  - document `room` on triggers and `set`
  - add `RoomNotFound` to the paragraph on `Ready` reasons
  - add a short note on Restart mode with room triggers
  - add the switch-off-when-empty pattern once the Pi check is done
- **`config/samples/home_v1alpha1_automation.yaml`**: add a `goodnight-living-room` Automation with a room action. Keep the hallway sample as it is until the Pi check.
- **`docs/deploy-raspberry-pi.md`**: no change. It doesn't cover writing automations.
- **"Not yet"** now points at the roadmap, so it doesn't need an edit.

## Open questions

1. **Does zigbee2mqtt re-report occupancy** while someone stays in front of one sensor? **Parked** with Pi bring-up. The user answers it with the third manual check. It blocks only the README's recommended Restart-mode pattern. The implementation and the conditional guidance above don't depend on it.
