# 0060: Unified rule model: moments and holds

> Replaces the `Schedule` kind of [0003](0003-schedules.md) and keeps its planner (#53). Approved in [#60](https://github.com/harrymitchinson/oikade/issues/60), which has the full discussion and a scenario-by-scenario walkthrough. Research: [Oikade rule model research](https://claude.ai/code/artifact/1405b549-7f2f-44ff-ba83-11d4b619e459).

## Problem

Oikade had two rule concepts that split the house's behaviour in awkward places. `Automation` reacts to capability changes, and `Schedule` (0003) is a stateful on/off driver. A humidity fan is a Schedule with no schedule in it, a motion light's off-delay is lost on a restart, and an Automation and a Schedule writing one device fight with no record of which won.

Done looks like this. Each case is written in Git with one mental model, is correct across a runtime restart, and is answerable with `kubectl`:

1. Porch light on 15 minutes before sunset.
2. Downstairs lights off at 23:30 on weeknights and 00:30 at weekends.
3. Water heater on 22:00–06:00. A restart at 02:00 still turns it on.
4. Bathroom fan on while humidity > 70%, with a 10-minute minimum run.
5. A pump on a daily budget (#19), with frost forcing it on.
6. Hall light on with motion, off 5 minutes after motion clears.
7. A one-off action that lasts a while: a towel-rail boost, or hallway lights at full brightness for 30 minutes.
8. A leak closes the water valve until someone resets it.
9. A wall switch or a Home app tap beats the rules for a while, and Oikade says which one happened.

## Decision

**One `Rule` kind, replacing `Automation`, with two shapes, plus opt-in arbitration per capability on `Device`.** (Option B of the issue. A: keep the split; C: B plus a `Signal` kind.) `while` is an embedded struct, so C's `while: {signal: …}` can be added later without changing any rule in Git.

- **`triggers`: moments.** `at`, `sun` or `change` triggers, with `condition`, `do`, `mode` and `startingDeadline`.
- **`while`: holds.** `window` (times or `{sun, offset}`), `when` (CEL), `budget` (the #53 planner, built in #19), `onDelay`, `offDelay` and `latch`, all ANDed, plus `hold: {device|room, capability, value}` and `priority`. Holds are recomputed from the clock and device state rather than timed in memory, so restarts are correct.
- **Arbitration.** A capability with a `Device.spec.control` entry is arbitrated. Rules, HomeKit and the device itself make claims, and the device takes the winner. Capabilities without an entry keep last-write-wins.

### Arbitration

| Level | Who claims | Ends |
|---|---|---|
| Safety | rules with `priority: Safety` | when the hold ends, or the set's `for`/`until` |
| Override | outside changes (HomeKit, or the device by itself), and rules with `priority: Override` | after `overrideHold`, or when its `until` says |
| Normal | ordinary rules | when the hold ends, or the set's `for`/`until` |
| default | nobody | never |

- The highest level with a claim wins. Within a level, the device's required `resolve` picks: `PreferOn`/`PreferOff` for bools, `Highest`/`Lowest` for numbers. Rules carry no numeric priorities.
- **Equipment limits** (`minimumOn`, `maximumOn`, `minimumOff`, `maximumStartsPerDay`) apply after the winner is picked, and win over every rule, Safety included.
- **Outside changes.** Status records the path a change came in by (`HomeKit`, `Device`, `Rule/<name>`), never "manual". The required `external` says what an outside change does: `Override` makes it a claim for `overrideHold` (a duration or `Next`); `Revert` writes the winner back at once. An accepted outside change counts towards the limits but isn't fought by them.
- **One-off sets** on an arbitrated capability are claims, and must end: `for: <duration>`, `until: Next` (when the result without the claim next changes), `until: Released`, or `for` with `until: Next`, ending at whichever comes first.
- **Latched holds** (`latch: true`) stay active once their condition has been true, until a `release` action. Being level-based, they can latch after a restart on a condition that became true while the runtime was down, as long as it's still true when the runtime next sees it. A condition that came and went during the downtime doesn't latch (answer 20).
- **Static conflicts.** Two `while` rules holding one unarbitrated capability set `Ready=False` on both. A `set` on an arbitrated capability without an end sets `Ready=False`. On a Room, arbitrated members of an endless `set` are skipped and listed in `Rule.status`.

```yaml
kind: Rule
metadata: {name: leak-shutoff}
spec:
  priority: Safety
  while: {when: 'devices["kitchen-leak"]["sensor.water-leak"]', latch: true}
  hold: {device: main-valve, capability: switch.on, value: false}
---
kind: Device
metadata: {name: main-valve}
spec:
  control:
  - {capability: switch.on, default: true, resolve: PreferOff, external: Revert}
```

`Device.status.control[]` then lists each capability's value, level and claims (`source`, `level`, `value`, `expires`, `latched`, `winner`, `reverted`). It changes at transitions, not on every value.

## Answered in review

1. **Opt-in arbitration:** only capabilities with a `control` entry are arbitrated.
2. **Timed `set`s on unarbitrated capabilities:** a plain write, with no claim.
3. **Safety versus limits:** limits always win, Safety included. This deliberately differs from BACnet. One rule with no exceptions is easier to reason about.
4. **Name:** `Automation` becomes `Rule`.
5. **Location:** runtime flags `--latitude`/`--longitude`, next to `--timezone`. One install is one home.
6. **0003:** this replaces `Schedule`.
7. **Ties within a level:** `resolve` per device, required. An optional numeric priority could be added later; removing one would break rules.
8. **One-off actions:** claims that end with `for`, `until: Next` or `until: Released`, and the `Override` level with `external`.
9. **A wall switch off during a hold:** with `overrideHold: Next` it lasts until the hold next changes. New motion doesn't end it.
10. **Restarts on unarbitrated holds:** the runtime writes only if the hold's state changed while it was down, against the persisted state.
11. **`days` after midnight:** `days` is the calendar day the time fires on.
12. **Safety while the runtime is down:** accepted. Rules run only while the runtime is up.
13. **Limits versus outside changes:** an outside change is accepted, counted and not fought. `external: Revert` opts out.
14. **Long `until: Next`:** may be combined with `for`, ending at whichever comes first.
15. **Latching:** `latch: true` on `while` holds.
16. **Sun times in windows:** `window.start`/`end` take `{sun: Sunrise|Sunset, offset}`.
17. **`triggers`, not `on`** (answered while building): kubectl decodes YAML 1.1, which reads a bare `on` key as the boolean `true`, so `spec.on` arrived as `spec.true`. Moments are `triggers[]`, as Automation's field was, and the issue's `on` reads as `triggers`.
18. **`PreferOn`/`PreferOff`, not `On`/`Off`** (answered while building): for the same reason, `resolve: On` arrived as `true`. The bool values of `resolve` are `PreferOn` and `PreferOff`, and the issue's `On`/`Off` read as those.
19. **The end of an unarbitrated number hold** (answered while building): the runtime writes nothing, and the device keeps the held value. A bool hold writes the opposite. Going back to a set value takes a `control` entry, whose `default` arbitration falls back to.
20. **Latches after downtime** (answered after building, by the user on 2026-10-21: keep it as built for now): a latch is set when the whole `while` is true and the hold is active, so after any `onDelay` and inside any `window`, judged from the device values the runtime has. After a restart, the runtime has a plugin device's value straight away, because plugins retain their state. It has a Zigbee2MQTT device's value only once the device next reports, because Oikade doesn't have Zigbee2MQTT retain device state. So a condition that became true and false again while the runtime was down, such as a short temperature spike, never latches. One that persists, such as a wet leak sensor, latches once the runtime sees it. Editing a hold's target (device, room, capability or value) also starts it afresh and drops its latch. Replaying missed changes, for example through a persistent MQTT session, is an idea for later.

## Consequences

- **Public API:** `Rule.spec.triggers`/`while`/`hold`/`priority`; `do[].set.for`/`until`/`priority` and `do[].release`; `Device.spec.control[]`. `Automation` is renamed, with a migration note (nothing is deployed outside Kind).
- **State that must survive:** hold active flags, latches, off-delay deadlines, claims with their expiries and `until: Next` baselines, released claims, limit counters and today's budget plan. They live in retained MQTT under `oikade/v1/_runtime/…`, and the plan in `status.today`.
- **Component boundaries:** the runtime owns arbitration and timers; the manager only validates. hap publishes its writes as HomeKit claims, and still writes devices directly, so HomeKit works when the runtime is down.
- **Metrics:** `oikade_automation_*` becomes `oikade_rule_*`; new `oikade_rule_active`, `oikade_rule_next_change_timestamp_seconds` and `oikade_device_control_level`.
- **Not in scope:** the `Signal` kind, re-planning mid-day, price forecasts and hysteresis on `when`.

## Arbiter behaviour chosen while building

`internal/control` implements the arbitration above. These choices weren't reviewed in #60, and are cheap to change before the runtime uses them:

- A `Revert` writes back the device's earlier value, exempt from the limits, and neither the outside change nor the write back counts towards them, since together they leave the device as it was. If the winner has changed to something else before the write back, the outside change stands and the limits apply as usual.
- A device first seen on isn't counted as a start, and its run counts from when it was first seen. A report of the value the device already has is no outside change.
- After `maximumOn` stops a device, it rests for `minimumOff`. Without a `minimumOff`, it stays off until the winner is off, so it can't cycle with no rest.
- Holding a number capability on (`minimumOn`) keeps its current value. Any non-zero value is on.
- A new outside change replaces earlier outside claims, so the latest one wins even within the Override level.
