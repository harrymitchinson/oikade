---
id: doc-0003
title: Schedules
type: specification
created_date: '2026-10-18 10:56'
updated_date: '2026-10-18 10:56'
---
**Decision:** accepted · **Work:** TASK-0003

## Problem

Several things in the house should run on a schedule, and they span a wide range:

- **Simple:** the water heater runs only during off-peak hours. A dehumidifier runs 08:00–10:00 on weekdays and 10:00–12:00 at weekends.
- **Complex:** the pool pump. It must filter for a number of minutes each day that depends on the water temperature: 2 hours in winter, 8 in a heatwave. Those minutes should land in off-peak hours, and during the preferred swimming hours so the water is clean, without short-cycling the pump. When there's a frost risk, the pump must run whatever the plan says.

Oikade can't express any of it. Automations fire only when a capability changes (`api/home/v1alpha1/automation_types.go` `Trigger`). They have no notion of time, no daily budget and no memory of how long something has run. Today the pool logic lives in the Homebridge plugin `../homebridge-plugins/plugins/sonoff-th-elite-pool`, whose config is outside Git and whose runtime never reaches Grafana. That plugin is the reference for the complex end: temperature bands, weighted windows, minimum run and rest, maximum starts, a frozen daily plan, credited runtime, and overrides from the Home app.

The user wants **one** flexible concept covering both ends, not a simple timer plus a separate pool planner. Inputs must be open-ended too. Fixed off-peak windows are what the user has today, but a daily tariff colour or a price forecast should fit later without a redesign.

Done looks like this:

- The water heater's off-peak schedule is a few lines of YAML.
- The pool's daily plan is declared in Git. `kubectl get schedules` shows today's target, the planned blocks and any shortfall.
- When the outdoor temperature drops below the frost threshold at 03:00, the pump turns on within a minute, even though the plan said off.
- Grafana shows target, planned and actually-run minutes per day.
- In the Home app, toggling the pump starts a 2-hour override, and an "automatic" switch pauses the schedule. Both behave as they do in the plugin today.
- A runtime restart mid-day doesn't re-plan the day or lose credited runtime.

## Constraints

- **Plane.**
  - The schedule definition is config (a CRD).
  - Today's frozen plan changes once a day, and credited runtime is checkpointed at most every 5 minutes, following the `lastSeen` precedent. Both are coarse enough for `status`, which also makes them survive a runtime restart.
  - The override expiry and the paused state are live state, so they go in retained MQTT. Mosquitto already persists retained messages to its PVC (`config/mosquitto/configmap.yaml`).
  - Minute-by-minute runtime goes into metrics.
- **Public API.**
  - A new `Schedule` kind in `home.oikade.dev`.
  - A new `Device` source, `schedule`, for the HomeKit control surface.
  - A CEL environment for rule conditions and the daily budget. It's a new language contract, so its variables are named carefully below.
  - New `oikade_schedule_*` metrics.
  - No new capability names. The control Device reports the existing `switch.on`.
- **State that must survive.** Nothing that forces a re-pair. The plan and credited runtime are restorable from `status` within the day. Losing them at worst re-plans the day. The control Device's AID follows the Device name, as every other Device's does.
- **Component boundaries.**
  - The **runtime** plans and executes. It's a single replica, so only one planner drives each target. It writes `Schedule.status`, as it already writes `Device` and `Automation` status.
  - The **manager** only validates (CEL compile, target exists).
  - **hap** exposes the control Device through the hub like any other Device, and writes to it over MQTT. It never writes to the API.
  - When the runtime is down, nothing switches on schedule, frost rules included. The relay itself stays controllable from the Home app. That's why frost rules are a convenience, not a fail-safe (see Non-goals).
- **Pi.** Planning runs once a day over at most 96 slots. Live rules are evaluated on input changes. Both cost nothing. There's no new pod.

## Options

### A. Do less: time triggers on Automations

Add `at` or `cron` triggers to Automations. Simple schedules become "at 22:00 on, at 06:00 off". The pool becomes several Automations with CEL conditions choosing between hard-coded windows.

- Pros: small, and also needed for "porch light at sunset".
- Cons:
  - It covers only the simple end. It can't count minutes run, place a budget, enforce minimum rest or maximum starts, or recover a block after a restart.
  - Two concepts would cover overlapping ground, which is exactly what the user doesn't want.
- Cost of reversing: low, but it doesn't meet the requirement. It's worth doing separately (see Non-goals).

### B. One `Schedule` kind built from rules, with an optional daily budget

A Schedule drives one writable bool capability on one Device. The day is divided into slots (`slotMinutes`). Every **rule** names a time `window` (start, end and days; the whole day if omitted), an optional CEL `when`, and one **effect**:

| Effect | Meaning |
|---|---|
| `require` | The target is on in these slots. |
| `forbid` | The target is never placed in these slots by the budget. |
| `prefer: N` | The budget favours these slots, with weight N (negative to avoid). |

Precedence is **require > forbid > prefer**, so a frost rule beats quiet hours.

- **Without `minutes`**, the target is on exactly in the required slots. That covers every simple schedule.
- **With `minutes`**, required slots count towards the budget first. The planner then places the rest in non-forbidden slots, by preference score, under `constraints`: minimum run, minimum rest, maximum starts per day. The planner is new Go code. The plugin (`scheduler/candidates.ts`) is only a reference for the behaviour.
- `minutes` is either a `bands` table over an input capability, with a fallback when the input is missing or stale, or a CEL `expression`.

**When rules are evaluated.**
- **Planning rules** (the default) are evaluated once a day at `planningTime`. `when` sees `devices` (current values), `now`, `tz` and `slot` (`start`, `hour`, `weekday`), and the result is frozen into `status.today`. Tariff colour, "skip weekends" and later forecasts all go here.
- **Live rules** (`live: true`, `require` only) are evaluated whenever an input changes. While `when` is true, the target is forced on regardless of the plan, and the runtime counts towards today's minutes. Frost risk goes here. Live rules never change the frozen plan, so the day stays predictable.

**Overrides.**
- Any change to the target that the runtime didn't command (the Home app, a wall switch, another Automation) becomes a force-on or force-off for `override.duration`. A live `require` still wins over a force-off.
- A control Device (`spec.schedule.scheduleRef`) shows in HomeKit as a Switch: `switch.on` means automatic scheduling is on. Turning it off forces off for the override duration. Turning it on clears the override. These are the plugin's semantics. The runtime publishes the control Device on the doc-0002 `oikade/v1` contract, so hap decodes it with no new code path.

- Pros:
  - One concept spans "on during off-peak" to the pool.
  - Flexibility comes from rules plus CEL, not from new planner types. A future `forecast` variable slots into `when` and `minutes` without an API change.
  - The plan is visible in `kubectl` and Grafana, and device I/O stays in drivers and plugins.
- Cons:
  - A new kind plus a CEL environment are real API surface.
  - A new planner has to be written and tested from scratch in Go.
  - Live rules add a second evaluation path that has to be tested against the frozen plan.
- Cost of reversing: medium. Schedules and their control Devices live in the user's repo, and removing the kind means rewriting them as Automations and losing budgets.

### C. Two kinds: a simple `Timer` and a pool-style `Planner`

The simple cases get a small kind with windows and days. The budget cases get a separate planner kind.

- Pros: each kind is easier to read on its own.
- Cons: two APIs for one idea. A schedule that grows ("off-peak only, but at least 2 hours, and never during swimming") has to be rewritten in the other kind. The shared parts (windows, days, overrides, the control Device) are built twice.
- Cost of reversing: high. Merging two kinds later means migrating users' YAML.

## Decision

**Accepted: B.** One kind with composable rules matches what the user asked for: simple schedules stay simple, and the pool is the same thing with a budget and more rules. Live `require` rules handle frost without giving up the predictability of a frozen daily plan. B gives up a small API surface, and takes on writing a new planner in Go, with the plugin as inspiration only.

Settled in review: a live rule that switches the target on keeps it on for at least `constraints.minimumRun`. That stops a frost rule at `< 1.0` from flapping around the threshold, without adding a per-rule setting.

The safety rules are also settled. Every schedule follows these, whatever its target:

- It switches the target on only when the Device is `Available=True` and has reported a value for the capability.
- It counts a start as soon as it sends the on command, whether or not the device confirms it.
- After a runtime restart where the target's state is unknown, it waits out `constraints.minimumRest` before switching on.
- There's no clock-rollback guard. The cluster keeps time with NTP.

There's no "target reached" surface in HomeKit. Progress shows in `kubectl get schedules` and in Grafana.

**Dependencies:**
- The control Device uses the doc-0002 contract and the driver-agnostic hub (TASK-0001.01 to TASK-0001.04).
- The pool needs the TH Elite plugin for the relay and the probe.
- Schedules on Zigbee targets need neither, except for the control Device.

A simple schedule:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Schedule
metadata:
  name: water-heater
spec:
  target: {device: water-heater, capability: switch.on}
  rules:
  - {name: off-peak, window: {start: "22:00", end: "06:00"}, require: true}
```

The pool:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Schedule
metadata:
  name: pool-filtration
spec:
  target: {device: pool-pump, capability: switch.on}
  planningTime: "00:00"            # local time in the runtime's --timezone
  slotMinutes: 30
  minutes:
    bands:                         # or: expression: '<CEL returning minutes>'
      input: {device: pool-probe, capability: sensor.temperature, maxAge: 30m}
      missingMinutes: 240
      ranges:
      - {below: 12, minutes: 120}
      - {below: 16, minutes: 240}
      - {below: 24, minutes: 360}
      - {minutes: 480}
  rules:
  - {name: off-peak, window: {start: "22:00", end: "06:00"}, prefer: 3}
  - {name: swimming, window: {start: "14:00", end: "19:00", days: [Sat, Sun]}, prefer: 2}
  - {name: quiet, window: {start: "07:00", end: "08:30"}, forbid: true}
  - name: frost
    live: true
    when: 'devices["outdoor"]["sensor.temperature"] < 1.0'
    require: true
  constraints:
    minimumRun: 1h
    minimumRest: 30m
    maximumStartsPerDay: 3
  override:
    duration: 2h
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata:
  name: pool-automatic
spec:
  displayName: Pool automatic
  schedule:
    scheduleRef: {name: pool-filtration}
```

**Public names (draft):**
- `Schedule` with `spec.target`, `planningTime`, `slotMinutes`, `minutes.bands` or `minutes.expression`, `rules[]` (`name`, `window{start,end,days}`, `when`, `live`, `require`, `forbid`, `prefer`), `constraints` and `override.duration`.
- `status.today`: `date`, `targetMinutes`, `scheduledMinutes`, `shortfallMinutes`, `blocks`, `runMinutes` and `starts`.
- Conditions `Ready` and `Planned`.
- CEL variables `devices`, `now`, `tz` and `slot`.
- Metrics `oikade_schedule_target_minutes`, `oikade_schedule_scheduled_minutes`, `oikade_schedule_run_minutes_total`, `oikade_schedule_override` and `oikade_schedule_live_rule_active`, each labelled `{namespace,schedule}`.

## Non-goals

- **A hardware fail-safe.** Frost rules run only while the runtime and the target's driver are up. Real frost protection stays in hardware, as the plugin's README insists.
- **Time triggers on Automations** (`at` or `cron`, sunset). They're useful, but they're a separate, smaller spec. Schedules drive a target's on/off. Automations do one-off actions.
- **Price forecasts in v1.** The `forecast` CEL variable and how plugins publish forecasts are a follow-up. v1 has windows, `when` over current values, and `slot`.
- **Multiple targets per Schedule**, setpoints rather than on/off, or schedules that coordinate with each other, such as a household power cap.
- **Re-planning mid-day.** The plan is frozen at `planningTime`, and changes to `spec` apply at the next boundary. Live `require` rules are the only mid-day input.

---

## API

### New kind: `Schedule`

```sh
kubebuilder create api --group home --version v1alpha1 --kind Schedule --resource --controller
```

This creates `api/home/v1alpha1/schedule_types.go` and `internal/controller/home/schedule_controller.go`. The field style follows `automation_types.go`. The CRD generator has no float support (`Makefile` doesn't set `allowDangerousTypes`), so band thresholds use `resource.Quantity`, which accepts `"12"`, `"0.5"` and `"-2"`.

```go
// TimeOfDay is a local wall-clock time, HH:MM, in the runtime's --timezone.
// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
type TimeOfDay string

// +kubebuilder:validation:Enum=Mon;Tue;Wed;Thu;Fri;Sat;Sun
type Weekday string

// TimeWindow is a daily window. An end at or before start wraps past midnight and belongs to the day it starts.
// +kubebuilder:validation:XValidation:rule="self.start != self.end",message="start and end must differ; omit window for the whole day"
type TimeWindow struct {
	// +required
	Start TimeOfDay `json:"start"`
	// +required
	End TimeOfDay `json:"end"`
	// days limits the window to these weekdays. Empty means every day.
	// +listType=set
	// +optional
	Days []Weekday `json:"days,omitempty"`
}

// ScheduleRule shapes when the target runs. Exactly one of require, forbid or prefer is set.
// +kubebuilder:validation:XValidation:rule="(self.require ? 1 : 0) + (self.forbid ? 1 : 0) + (has(self.prefer) ? 1 : 0) == 1",message="exactly one of require, forbid or prefer must be set"
// +kubebuilder:validation:XValidation:rule="!self.live || (self.require && has(self.when))",message="live rules must set require and when"
type ScheduleRule struct {
	// name identifies the rule in status, logs and metrics.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	// +required
	Name string `json:"name"`
	// window limits the rule to a time of day. Omitted means the whole day.
	// +optional
	Window *TimeWindow `json:"window,omitempty"`
	// when is a CEL condition. Planning rules see devices, now, tz and slot. Live rules see devices, now and tz.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	When string `json:"when,omitempty"`
	// live re-evaluates the rule whenever an input changes, instead of once at planningTime.
	// +optional
	Live bool `json:"live,omitempty"`
	// +optional
	Require bool `json:"require,omitempty"`
	// +optional
	Forbid bool `json:"forbid,omitempty"`
	// prefer weights the slots for the budget. Negative values avoid them.
	// +kubebuilder:validation:Minimum=-100
	// +kubebuilder:validation:Maximum=100
	// +optional
	Prefer *int32 `json:"prefer,omitempty"`
}

// CapabilityRef names a capability on a Device in the same namespace.
type CapabilityRef struct {
	// +kubebuilder:validation:MinLength=1
	// +required
	Device string `json:"device"`
	// +kubebuilder:validation:MinLength=1
	// +required
	Capability string `json:"capability"`
}

// MinutesInput is a capability read at planningTime.
type MinutesInput struct {
	CapabilityRef `json:",inline"`
	// maxAge is how old the value may be. Older values count as missing.
	// +kubebuilder:default="30m"
	// +optional
	MaxAge metav1.Duration `json:"maxAge,omitzero"`
}

// MinutesBand maps input values below a threshold to a daily budget. The last band has no threshold.
type MinutesBand struct {
	// +optional
	Below *resource.Quantity `json:"below,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1440
	// +required
	Minutes int32 `json:"minutes"`
}

type MinutesBands struct {
	// +required
	Input MinutesInput `json:"input"`
	// missingMinutes is the budget when the input is missing or older than maxAge.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1440
	// +required
	MissingMinutes int32 `json:"missingMinutes"`
	// ranges must ascend by below, and only the last omits below.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +required
	Ranges []MinutesBand `json:"ranges"`
}

// ScheduleMinutes sets the daily budget. Exactly one of bands or expression is set.
// +kubebuilder:validation:XValidation:rule="has(self.bands) != has(self.expression)",message="exactly one of bands or expression must be set"
type ScheduleMinutes struct {
	// +optional
	Bands *MinutesBands `json:"bands,omitempty"`
	// expression is CEL over devices, now and tz, returning minutes as an int.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Expression string `json:"expression,omitempty"`
}

type ScheduleConstraints struct {
	// minimumRun is the shortest planned block, and the shortest run after a live rule switches the target on.
	// +optional
	MinimumRun metav1.Duration `json:"minimumRun,omitzero"`
	// minimumRest is the shortest gap between runs.
	// +optional
	MinimumRest metav1.Duration `json:"minimumRest,omitzero"`
	// maximumStartsPerDay limits starts per planning day. Zero means unlimited.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaximumStartsPerDay int32 `json:"maximumStartsPerDay,omitempty"`
}

type ScheduleOverride struct {
	// duration is how long a manual change, or turning automatic off, holds the target.
	// +kubebuilder:default="2h"
	// +optional
	Duration metav1.Duration `json:"duration,omitzero"`
}

// ScheduleSpec decides when one writable bool capability is on.
type ScheduleSpec struct {
	// target is the capability the schedule drives, e.g. switch.on.
	// +required
	Target CapabilityRef `json:"target"`
	// planningTime starts each planning day.
	// +kubebuilder:default="00:00"
	// +optional
	PlanningTime TimeOfDay `json:"planningTime,omitempty"`
	// +kubebuilder:validation:Enum=15;30;60
	// +kubebuilder:default=30
	// +optional
	SlotMinutes int32 `json:"slotMinutes,omitempty"`
	// minutes is the daily budget. Without it, the target is on exactly in required slots.
	// +optional
	Minutes *ScheduleMinutes `json:"minutes,omitempty"`
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Rules []ScheduleRule `json:"rules,omitempty"`
	// +optional
	Constraints ScheduleConstraints `json:"constraints,omitzero"`
	// +optional
	Override ScheduleOverride `json:"override,omitzero"`
}

type ScheduleBlock struct {
	Start metav1.Time `json:"start"`
	End   metav1.Time `json:"end"`
}

// ScheduleDay is the frozen plan for one planning day, plus coarse progress.
type ScheduleDay struct {
	// date is the planning day, YYYY-MM-DD, local time.
	Date string `json:"date"`
	// targetSource is Bands, Missing, Expression or None.
	TargetSource     string          `json:"targetSource"`
	TargetMinutes    int32           `json:"targetMinutes"`
	ScheduledMinutes int32           `json:"scheduledMinutes"`
	ShortfallMinutes int32           `json:"shortfallMinutes"`
	Blocks           []ScheduleBlock `json:"blocks,omitempty"`
	// runMinutes is the credited runtime so far, checkpointed at most every 5 minutes.
	RunMinutes int32 `json:"runMinutes"`
	// starts counts on commands sent today.
	Starts int32 `json:"starts"`
}

type ScheduleStatus struct {
	// +optional
	Today *ScheduleDay `json:"today,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// conditions: Ready (manager) and Planned (runtime).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

Printer columns are `Target` (`.spec.target.device`), `Today` (`.status.today.targetMinutes`), `Run` (`.status.today.runMinutes`), `Ready` and `Planned`.

The conditions are:

| Type | Reason | Status | Meaning |
|---|---|---|---|
| `Ready` (manager) | `Valid` | True | The spec compiles and the target exists. |
| | `Invalid` | False | See the compile rules below. The message names the field. |
| | `TargetNotFound` | False | No Device with that name exists in the namespace. |
| `Planned` (runtime) | `Satisfied` | True | Today's plan meets the target. |
| | `Infeasible` | False | The constraints leave a shortfall, given in the message. The plan still runs. |
| | `Waiting` | Unknown | There's no plan yet, for example straight after the Schedule is created or the runtime restarts. |

The compile rules, which the manager reports as `Invalid` and the runtime refuses to load, are:

- The target capability must be in the registry with Kind `Bool` and `Writable`.
- Every CEL expression must compile. `when` must return a bool, and `minutes.expression` must return an int.
- `prefer` and `forbid` rules need `spec.minutes`.
- `minimumRun`, `minimumRest`, and every window start and end must be multiples of `slotMinutes`. So must `planningTime`.
- Bands must ascend, and only the last omits `below`.

### `Device.spec.schedule`

```go
// ScheduleDeviceSource makes the Device the HomeKit control surface of a Schedule.
type ScheduleDeviceSource struct {
	// +required
	ScheduleRef corev1.LocalObjectReference `json:"scheduleRef"`
}
```

`DeviceSpec` gains `Schedule *ScheduleDeviceSource \`json:"schedule,omitempty"\``. The source rule from doc-0002 grows to `[has(self.zigbee2mqtt), has(self.plugin), has(self.schedule)].filter(x, x).size() == 1`. If Plugin drivers hasn't landed yet, the rule lists `zigbee2mqtt` and `schedule`, and TASK-0001.03 adds `plugin`.

### The CEL environment (`internal/schedule/cel.go`)

| Variable | Type | Planning rules | Live rules | `minutes.expression` |
|---|---|---|---|---|
| `devices` | map, Device to capability to value | ✓ (values at planningTime) | ✓ (current) | ✓ |
| `now` | timestamp | ✓ | ✓ | ✓ |
| `tz` | string | ✓ | ✓ | ✓ |
| `slot` | map: `start`, `end` (timestamps), `hour`, `minute`, `weekday` (0 = Sunday) | ✓ | | |

A rule's `window` and its `when` are ANDed. So `when` can do what a window can't express, such as `slot.weekday == 3 && devices["tariff"]["sensor.tariff-red"] != true`.

## Behaviour

### The planner (`internal/schedule/plan.go`): pure, with no I/O

`Plan(in PlanInput) Day` takes the compiled spec, the slot boundaries for the day in the runtime's timezone (DST days have 23 or 25 hours, so they have fewer or more slots), a `notBefore` time (slots that start earlier are unavailable), the values in `devices`, and the rest and start state carried over from the previous day.

1. **Budget.**
   - `bands`: if the input is missing or older than `maxAge`, use `missingMinutes` (source `Missing`). Otherwise use the first band whose `below` exceeds the value (source `Bands`).
   - `expression` gives source `Expression`.
   - No `minutes` gives source `None`.
   - Clamp the result to between 0 and 1440, and round it up to a whole number of slots.
2. **Classify each slot.** Evaluate the planning rules whose window covers any minute of the slot, and whose `when` is true for that slot. If any `require` rule matches, the slot is *required*. Otherwise, if any `forbid` rule matches, it's *forbidden*. Otherwise its score is the sum of the matching `prefer` weights.
3. **No budget:** the plan is the required slots, merged into blocks. Constraints don't apply, because the user asked for exactly these windows.
4. **With a budget:** required slots are fixed blocks and count towards the budget. Choose the remaining slots from the allowed (not forbidden) ones, to maximise total score, subject to these rules:
   - every block, including any required slots it extends, lasts at least `minimumRun`
   - gaps between blocks are at least `minimumRest`, including against the carried-over rest at the start of the day
   - starts stay within `maximumStartsPerDay`
   - the total doesn't exceed the target

   Search slot by slot, keeping the best plan for each combination of minutes used and starts made. Break ties the same way every time: highest score, then fewest starts, then longer blocks, then earlier blocks. That way the same inputs always give the same plan.
5. If required slots already exceed the budget, the plan is just those slots, with a shortfall of 0. Any shortfall is reported, and the plan still runs.

### Runtime: the `schedule.Runner` (`internal/schedule/runner.go`, set up in `internal/runtime/runtime.go`)

1. **Load.** A `Loader` reconciler, like `engine.Loader`, compiles each Schedule and hands it to the Runner. It replaces the compiled spec on a generation change. The new spec applies at the next planning boundary. The exception is a Schedule with no plan for today, which is planned straight away.
2. **Plan.** At each `planningTime` (a `clock.Clock` timer, the same clock the engine uses), or on startup when `status.today.date` isn't today, call `Plan` with `notBefore` set to now. Write `status.today` and `Planned` immediately. If `status.today.date` is today, reuse the frozen blocks, `runMinutes` and `starts`. A runtime restart never re-plans the same day.
3. **Decide what the target should be.** Re-evaluate at every slot edge, on every change to the target or a live-rule input, and when an override expires. Precedence runs from highest to lowest:
   1. an active **live** `require` rule, or a live-started run still inside its `minimumRun`: on
   2. **override**: force-on or force-off
   3. **the plan**: on inside a block, off otherwise
4. **Safety rules,** applied before any on command. If one blocks, the Runner re-checks on the next evaluation, and counts it in `oikade_schedule_blocked_total{reason}`.
   - The target Device must be `Available=True` (`hub.Snapshot.Online`) and have reported the capability (reason `unavailable`).
   - The last off must be at least `minimumRest` ago (reason `resting`). After a restart where the target hasn't reported yet, rest counts from the runtime's start. Live rules obey this too: the pump's protection wins over a convenience frost rule.
   - `starts` must be below `maximumStartsPerDay`, when that's set (reason `max_starts`). Live rules **ignore** this limit.
   - Off commands are never blocked.
5. **Commands** go through `hub.Set`. A start counts as soon as the on command is sent. The Runner remembers each command for 30 s, to tell its own changes from external ones.
6. **Crediting runtime.** For each minute in which the target reported `true` and the Device was available, add one minute. It counts whatever caused the run: plan, live rule or override. Write `runMinutes` and `starts` to `status.today` at most every 5 minutes, and immediately at block edges and at the planning boundary. The writer is rate-limited like `engine.Triggered`.
7. **External changes.** A change to the target that doesn't match a command from the last 30 s becomes an override in that direction, lasting `override.duration`. It might come from the Home app, a wall switch or an Automation. The Runner logs it as `"Started Schedule override"`, with `source=external`.

### Runtime: the control Device

1. The runtime acts as a publisher on the doc-0002 contract, under the reserved root `oikade/v1/_runtime/schedules`. Kubernetes namespaces can't start with `_`, so it can't collide with a namespace. Device ids are `<namespace>.<schedule>`.
   - `status`: retained, with a Last Will. This needs a `mqtt.WithWill` option on `mqtt.Connect` (`internal/mqtt/client.go`), using autopaho's will message.
   - `devices`: one entry per Schedule, `capabilities: ["switch.on"]`, manufacturer `Oikade`, model `Schedule`, and `undeclared: 0`. A Schedule without a control Device is normal.
   - `<id>/state`: `{"switch.on": automatic}`.
   - `<id>/availability`: `online`.
2. The hub has a schedule-source reconciler, like the Plugin one in doc-0002. It feeds the doc-0002 decoder with root `oikade/v1/_runtime/schedules`, and declared ids taken from `Device.spec.schedule.scheduleRef`. It runs in the runtime and in every hap pod. Its `InstanceKey` is `{Driver: "schedule", <namespace>/_schedules}`.
3. **Writes** arrive on `<id>/set`:
   - `{"switch.on": false}` (automatic off) starts a force-off override for `override.duration`, with `source=homekit`.
   - `{"switch.on": true}` clears any override.
   - `automatic` is true exactly when no force-off override is active. A force-on from an external change leaves automatic true.
4. **Override state** is the runtime's own data, not part of the contract. It's retained at `oikade/runtime/schedules/<namespace>/<name>/override` as `{"mode":"on|off","until":"<RFC3339>","source":"external|homekit"}`, and read back on startup. An expired or missing record means no override.

### Manager: `ScheduleReconciler`

1. Compile. On error, set `Ready=False Invalid`.
2. `Get` the target Device. If it's missing, set `TargetNotFound`.
3. Otherwise, set `Ready=True Valid`.
4. Watch Devices, mapping a Device to every Schedule in its namespace, so that creating the target flips `Ready`.

### Failure modes

| Situation | What happens | What the user sees |
|---|---|---|
| The runtime is down | Nothing switches on schedule, and live rules don't run. The target keeps its last state. The control Device's status goes offline through the Last Will. | `Pool automatic` shows not responding in the Home app. The pump stays controllable. |
| The runtime restarts mid-block | It reuses the frozen plan and `runMinutes` from status. At most 5 minutes of credited runtime is lost. If the target is reported on, the block continues; otherwise the rest rule applies first. | `Planned` stays as it was. |
| The broker restarts | Retained override records and control states come back, because Mosquitto persists them. Commands fail while the broker is down, and are retried at the next evaluation. | `oikade_schedule_blocked_total` doesn't move, because off and on commands fail as errors in the log. |
| The target is offline at block start | The start is blocked (`unavailable`) and re-checked on every change. If it comes back mid-block, the remainder runs. | `oikade_schedule_blocked_total{reason="unavailable"}`. Shortfall shows in `runMinutes` against `targetMinutes`. |
| The input sensor is stale at planningTime | The budget uses `missingMinutes`. | `targetSource: Missing`. |
| A live rule's input disappears | `when` errors or is false, so the live rule is inactive. It's logged once per rule. | `oikade_schedule_live_rule_active=0`, plus the log. |
| The frost rule flaps around its threshold | A live-started run holds for `minimumRun`. Rest still applies. | Smooth on/off. |
| The Schedule is edited mid-day | The new spec applies at the next boundary, unless there's no plan yet. | `observedGeneration` updates. `status.today` stays until the next planning day. |
| The target Device is deleted | `Ready=False TargetNotFound`. The Runner stops commanding. | Condition. |
| Two Schedules target the same capability | They fight. The manager rejects the second one by name with `Ready=False Invalid` ("target already driven by Schedule X"); the first by name wins. | Condition. |
| A DST change | The planning day has 23 or 25 hours, and the slots follow the local clock. | Correct local times in `status.today.blocks`. |
| A clock jump | Not guarded, as decided. Blocks are absolute timestamps, so a jump shifts which block is current. | Possibly a missed or extra block for that day. |

## State and upgrade

- **What persists:**
  - `Schedule.status.today`: the plan, `runMinutes` and `starts`. Losing it re-plans the rest of the day.
  - The retained override records in Mosquitto's PVC. Losing them clears overrides, which falls back to automatic.

  Nothing here needs a backup or forces a re-pair. The control Device's AID follows its Device name in `Bridge.status.accessories`, as for every Device.
- **Upgrade:** additive. There's a new CRD, a new optional `Device` field, and a source rule every existing Device satisfies. `mqtt.WithWill` is opt-in, and only the runtime uses it.
- **Deleting a Schedule:** the Runner stops, and the target keeps its last state. The Runner doesn't switch it off, because that's an unasked-for change to a real device. The override record is deleted, and the control Device goes `Resolved=False NoDriver`.
- **Restoring from Git:** Schedules and control Devices come back. The first planning pass plans the rest of the day, because the status was lost.

## Observability

These are collected by the runtime, in a `schedule.Collector`:

| Metric | Type | Labels | |
|---|---|---|---|
| `oikade_schedule_target_minutes` | gauge | `namespace`, `schedule` | Today's budget |
| `oikade_schedule_scheduled_minutes` | gauge | `namespace`, `schedule` | Minutes in today's plan |
| `oikade_schedule_run_minutes_total` | counter | `namespace`, `schedule` | Credited minutes. Use `increase(…[1d])` for daily totals. |
| `oikade_schedule_desired` | gauge | `namespace`, `schedule` | 1 while the Runner wants the target on |
| `oikade_schedule_override` | gauge | `namespace`, `schedule` | -1 force-off, 0 none, 1 force-on |
| `oikade_schedule_live_rule_active` | gauge | `namespace`, `schedule`, `rule` | 1 while a live rule's `when` is true |
| `oikade_schedule_blocked_total` | counter | `namespace`, `schedule`, `reason` | `unavailable`, `resting`, `max_starts` |

`desired` and `blocked_total` weren't in the top half's draft list. They're added so you can see why the target isn't on.

The log messages are:

- `"Planned Schedule day"`, with `targetMinutes`, `scheduledMinutes`, `shortfallMinutes` and `blocks`
- `"Switched Schedule target"`, with `value` and `cause` (`plan`, `live`, `override`)
- `"Started Schedule override"` and `"Ended Schedule override"`, with `mode` and `source`
- `"Blocked Schedule start"`, with `reason`, at most once per slot

The Grafana dashboard gets a "Schedules" row:

- daily target against credited minutes, as bars
- `desired` against the target's `oikade_capability_value`, as a state timeline
- overrides and active live rules

## Security

- **Manager RBAC** (markers on `ScheduleReconciler`): `schedules` get, list and watch, and `schedules/status` get, update and patch, in `home.oikade.dev`. Devices get, list and watch is already granted.
- **Runtime:** `config/runtime/role.yaml` gains `schedules` get, list and watch, and `schedules/status` update and patch.
- **hap:** no change. The schedule-source hub reconciler reads only Devices, which it can already read, and talks to the bus.
- **The broker is still anonymous.** Anything on the cluster network could publish to `oikade/v1/_runtime/schedules/<id>/set` and pause a Schedule, just as it can toggle any Device today. Scoping that waits for MQTT auth. No Secrets are involved.

## Test plan

| Behaviour | Layer | Test |
|---|---|---|
| Budget from bands, missing or stale input, expression, clamping and rounding | Unit | `internal/schedule` `TestBudget` |
| Slot classification: require > forbid > prefer, windows wrap midnight, days, `when` with `slot` | Unit | `TestClassify` |
| No budget gives exactly the required blocks | Unit | `TestPlanRequiredOnly` |
| The plan never breaks minimum run, minimum rest, maximum starts or forbidden slots, and never exceeds the budget | Unit | `TestPlanConstraints`, table-driven, plus a randomised property test over generated specs |
| The plan maximises score, with deterministic tie-breaks | Unit | `TestPlanOptimal` (brute-force comparison on small days), `TestPlanTieBreak` |
| Shortfall and the infeasible case | Unit | `TestPlanShortfall` |
| DST days have 46 or 50 slots at 30 minutes | Unit | `TestPlanDST` (Europe/Paris, both changes) |
| Precedence: live > override > plan | Unit | `TestRunnerPrecedence`, with a fake clock and a fake `Setter` |
| Safety rules: unavailable, resting after restart, max starts (ignored by live) | Unit | `TestRunnerSafety` |
| A live start holds for `minimumRun` | Unit | `TestRunnerLiveMinimumRun` |
| Crediting runtime only while on and available, and checkpointing | Unit | `TestRunnerCredit` |
| An external change becomes an override, and the runner's own commands don't | Unit | `TestRunnerExternalOverride` |
| A restart reuses today's plan and `runMinutes` | Unit | `TestRunnerResume` |
| Bus round trip: plan, `/set` on the target, credited status | Integration | `internal/runtime` with `mqtttest` and a fake clock |
| Control Device: the contract topics are published, automatic off gives force-off, retained override survives a runner restart | Integration | `mqtttest` |
| CEL rules (effects, live, bands, minutes) | envtest | `internal/controller/home` |
| `Ready`: `Valid`, `Invalid` (compile, slot multiples, duplicate target), `TargetNotFound`, then `Valid` | envtest | `schedule_controller_test.go` |
| Deployed: a Schedule with a required window drives a faked z2m plug | Kind e2e | `test/e2e`: shorten the window to the next slot, assert `zigbee2mqtt/<ieee>/set` and `Planned=True` |

Manual checks on the Pi (after the TH Elite plugin exists for the pool parts):

1. **Simple schedule.** The water heater's off-peak rule switches on at 22:00 and off at 06:00. Pass: the `/set` publishes are within 1 minute of each edge.
2. **Pool plan.** At planning time, `kubectl get schedule pool-filtration` shows a target that matches the probe's band, and blocks inside off-peak and swimming hours. Pass: credited minutes reach the target by the end of the day, with the Grafana bar matching.
3. **Frost.** Temporarily set the frost rule to `< 30.0`. Pass: the pump turns on within 1 minute, and stays on for at least `minimumRun` after reverting.
4. **Home app.** Toggling the pump on outside a block holds it on for 2 hours, and then the plan resumes. Turning `Pool automatic` off holds the pump off for 2 hours, unless frost.
5. **Restart.** `kubectl rollout restart deploy/oikade-runtime` mid-block. Pass: the block continues, `runMinutes` loses at most 5, and there's no re-plan.

## Docs

- **README:**
  - add `Schedule` to the kinds table, and the schedule source to the `Device` description
  - add a "Schedules" section after "Automations", with the water heater and pool examples, the rule effects and precedence, live rules, overrides and the safety rules
  - add the seven metrics to Metrics
  - add a note under "State that must survive" that schedule state is reconstructable and needs no backup
- **`config/samples/home_v1alpha1_schedule.yaml`**: the water heater and the pool, plus a `pool-automatic` control Device.
- **`docs/deploy-raspberry-pi.md`**: the runtime's `--timezone` matters for schedules, and moving the pool from Homebridge to a Schedule.
- **`docs/plugin-contract.md`** (from doc-0002): note the reserved `_runtime` root.

## Open questions

None.
