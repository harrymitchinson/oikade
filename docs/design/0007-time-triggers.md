# 0007: Time triggers on Automations

> Written by another agent session and not yet reviewed. Work: [#39](https://github.com/harrymitchinson/oikade/issues/39).

## Problem

Some actions in the house happen at a time, not because a device changed: the porch light comes on 15 minutes before sunset, the downstairs lights go off at 23:30, and the bedroom blinds close at dusk. Oikade can't express any of these. An Automation only starts when a capability changes (`api/home/v1alpha1/automation_types.go` `Trigger`), and `now` and `tz` are only visible inside the condition once something else has fired.

Schedules ([0003](0003-schedules.md)) handle "keep this bool on during these windows" for one target. They left out one-off actions on purpose ([0003](0003-schedules.md) Non-goals): setting a level, acting on a whole room, or an action that runs at sunset when sunset moves every day.

Today the only workaround is a HomeKit automation in the Home app. That lives outside Git, needs a home hub, and never shows up in Grafana.

Done looks like this:

- "Porch light on 15 minutes before sunset" is a single trigger in an Automation in Git.
- The action runs within a second of the planned time. `oikade_automation_runs_total` counts it, and Loki logs it, like any other run.
- A Prometheus gauge says when each time-triggered Automation will next fire, so "will the porch light come on tonight?" can be answered without waiting.

## Constraints

- **Public API.** This adds new trigger fields to `Automation.spec.triggers` and a new `trigger.type` key in CEL. Both must be additive: every existing Automation must keep its meaning, and conditions such as `trigger.value && …` must still work for capability triggers.
- **Plane.** Clock times and offsets are config, so they go in the CRD. Today's computed sunset time is derived, so it never goes into etcd. It goes in a metric.
- **Component.** The runtime owns the timers, because it already runs the engine with a `clock.Clock` and `--timezone`. The manager only validates the spec. HomeKit isn't affected. When the runtime is down, time triggers don't fire, which is the same as every other Automation.
- **Consistency with Schedules.** Reuse [0003](0003-schedules.md)'s `TimeOfDay` (`HH:MM` in the runtime's `--timezone`) and `Weekday` (`Mon`…`Sun`), so the two kinds read the same.
- **Pi.** Computing sun times is pure arithmetic in native Go. It needs no network, no cloud API and no new image.

## Options

### A. Time and sun triggers on Automation (recommended)

`Trigger` becomes a union of three kinds: the existing capability trigger, `time: {at, days}` and `sun: {event, offset, days}`. The engine works out the next fire time for each trigger, keeps one timer on its `clock.Clock` for the earliest, and sends each fire through the same path as a capability change. `mode`, the condition and the run metrics then behave exactly as they do now. Sun times come from a small `internal/sun` package that takes the latitude and longitude from new runtime flags `--latitude` and `--longitude`, next to `--timezone`.

- Pros: one concept ("when X, if Y, do Z") with a few more kinds of X. Room actions, levels and delays all work unchanged. The change is small: one API struct, one engine timer and one pure package.
- Cons: the household's location becomes a runtime flag that the manager can't see. So the manager can't report "sun trigger with no location" on `Ready`, and the runtime has to report it instead.
- Cost of reversing: low. The fields are additive and optional. Removing them later would break only the Automations that use them.

### B. A built-in `sun` Device

The runtime acts as a virtual driver. It publishes a `sun` Device with the capabilities `sun.up` (bool) and `sun.elevation` (degrees). "At sunset" becomes a capability trigger on `sun.up` with `!trigger.value`.

- Pros: no new trigger kind. Sun state shows up in Grafana and in `devices` for any condition. HomeKit could even show it.
- Cons: offsets turn into elevation thresholds ("-15m" becomes `sun.elevation < 2`), which nobody can read at a glance. Clock times ("23:30") still aren't solved, and a `clock` device that ticks every minute would flood the bus and history. It also adds two public capability names and a non-hardware driver.
- Cost of reversing: medium. Capability names end up in PromQL and Automations.

### C. Do less: Schedule windows and HomeKit

On/off pairs ("porch light from sunset to 23:30") are Schedules, with sun-relative window bounds added to [0003](0003-schedules.md). Everything else stays in the Home app.

- Pros: no Automation change, and it reuses the Schedule runner.
- Cons: it doesn't cover the stated cases: a level, a room action, a single action with no matching "off", or anything that should go through an Automation's condition. HomeKit automations stay outside Git and Grafana. It also makes Schedule windows more complex before the pool even runs on them.
- Cost of reversing: low, but the problem stays unsolved.

## Decision

**Option A.** It's the smallest change that covers every case in the Problem section, and it keeps one mental model: a trigger is just a new kind of "when". B looks neat for sunset, but it fails on clock times and makes offsets unreadable. C defers the problem and puts more load on Schedules. In exchange, A puts the location in a runtime flag the manager can't check. The runtime reports a missing location in a log line and a metric rather than on `Ready` (see Open questions).

What you'd commit:

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Automation
metadata:
  name: porch-light-evening
spec:
  triggers:
    - sun: {event: Sunset, offset: -15m}
  actions:
    - set: {device: porch-light, capability: light.on, value: true}
---
apiVersion: home.oikade.dev/v1alpha1
kind: Automation
metadata:
  name: downstairs-off-at-night
spec:
  triggers:
    - time: {at: "23:30", days: [Sun, Mon, Tue, Wed, Thu]}
    - time: {at: "00:30", days: [Sat, Sun]}     # Friday and Saturday nights
  actions:
    - set: {room: living-room, capability: light.on, value: false}
    - set: {room: kitchen, capability: light.on, value: false}
```

```yaml
# config/runtime/deployment.yaml
- --timezone=Europe/London
- --latitude=51.5
- --longitude=-0.12
```

Public names it introduces:

- **`Trigger.time`**: `at` (`TimeOfDay`, required) and `days` (`[]Weekday`, optional, defaulting to every day).
- **`Trigger.sun`**: `event` (`Dawn`, `Sunrise`, `Sunset` or `Dusk`; dawn and dusk are civil twilight, −6°), `offset` (a signed duration, within ±6h) and `days`.
- **Trigger validation.** Exactly one of `capability`, `time` or `sun` is set. `device` and `room` are only allowed with `capability`. `capability` goes from required to optional, which relaxes validation without breaking anything that exists.
- **CEL.** `trigger.type` is `"capability"`, `"time"` or `"sun"`, and is always present. Time and sun triggers also set `trigger.at`, the planned fire time as a timestamp. They have no `device`, `capability`, `value` or `previous`. A condition shared by both kinds of trigger checks `trigger.type` first.
- **Runtime flags.** `--latitude` and `--longitude` (decimal degrees). They're only needed for sun triggers.
- **Metric.** `oikade_automation_next_trigger_timestamp_seconds{namespace, automation}` is a gauge with the earliest planned time or sun fire. It's absent when there's nothing to plan.

## Non-goals

- **Cron expressions.** `at` with `days` covers the cases in the house. A `cron` field could be added later without breaking anything.
- **Catching up on missed fires.** If the runtime is down at 23:30, that run is skipped. When it restarts, it plans from now and never replays fires it missed.
- **Sun-relative Schedule windows.** These are worth doing later, because they reuse `internal/sun`, but they're [0003](0003-schedules.md)'s API.
- **Per-Automation location or timezone.** There's one household, so there's one location and one timezone.
- **A `Home` or site CRD holding the timezone and location.** See Open questions.

---

## API

TODO (write-spec)

## Behaviour

TODO (write-spec): DST gaps and repeats, polar days with no sunset, offsets that cross midnight, reload while a timer is pending

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

## Open questions

1. **Where does the location live?** Runtime flags are the smallest change and match `--timezone`. But the manager can't then set `Ready=False` for a sun trigger when no location is configured. The alternative is a single `Home` (site) object holding the timezone and location, which both the manager and the runtime read. That's a new kind, and it's easier to add before Schedules ship than after. *Assumption: flags. A missing location gets one log line when the Automation loads, and sun triggers don't fire.*
2. **Should `TimeOfDay` and `Weekday` land with this work or with Schedules ([#21](https://github.com/harrymitchinson/oikade/issues/21))?** Whichever ships first defines them in `api/home/v1alpha1`. *Assumption: Schedules first, since they're already on the board.*
3. **DST.** For a non-existent local time (01:30 on the spring-forward day), the proposal is to fire at the first valid instant after it. For a repeated time (01:30 on the fall-back day), fire once, at the first occurrence. Confirm that's what you'd expect.
