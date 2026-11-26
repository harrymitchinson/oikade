# Writing flows

A flow is an automation written as ordinary Go, with real `if`, `for` and `switch`. It can wait for hours, and it survives a crash at any point. Use a [decision](automations.md#decisions) when something should stay true, and a flow when something should happen. [Testing a home](testing.md#tests) shows how to test one on a fake clock, crashes included.

```go
// Downstairs is a room, and downstairs its lights, from home.Room(Downstairs, oikade.Lights).
var (
	Downstairs = oikade.Room("downstairs")
	downstairs oikade.Group
)

func bedtime(r *oikade.FlowRun) {
	r.Set(downstairs.Brightness.To(20))
	r.Sleep(30 * time.Minute)

	guests, err := r.Do("calendar", calendar.HasGuests) // func(context.Context) (bool, error)
	if err != nil {
		r.Fail(err)
	}
	if guests {
		r.Sleep(time.Hour)
	}
	r.Set(LoungeLamp.On.To(false))
}

func main() {
	home := oikade.NewHome(oikade.Options{})
	downstairs = home.Room(Downstairs, oikade.Lights)
	home.Flow("bedtime", bedtime).At(oikade.Clock("22:30"))
	home.Run()
}
```

`home.Flow` adds a flow, under a name you give it once.

## Triggers

A flow needs a trigger: `When`, or a time of day in `At`.

```go
home.Flow("hallway-motion-light", func(r *oikade.FlowRun) {
	r.Set(HallwayLight.On.To(true))
	r.Sleep(5 * time.Minute)
	r.Set(HallwayLight.On.To(false))
}).
	Restart(). // new motion restarts the 5 minutes
	When(HallwaySensor.Occupied.Becomes(true)).
	If(oikade.Daily("18:00", "07:00"))
```

| Method | |
|---|---|
| `When(changes…)` | The changes that start it. A `Bool`, a `Number` or a `Choice` fires whenever it changes, and a `Bool`'s `Becomes(true)` only when it changes to true. A [group's or a room's](automations.md#triggers-and-writes) fires on a change from any of its members, and a [signal](automations.md#signals) as it changes. |
| `At(times…)` | [Times of day](automations.md#times-of-day) it starts at, instead of or as well as `When`. `At(Clock("18:00"), Sunset(0))` fires at both, and the same time twice fires once. |
| `If(cond)` | A [condition](automations.md#conditions), looked at when a trigger fires. The run starts only if it's true. |
| `Restart()` | A trigger while a run is in progress cancels that run, and what it had recorded, and starts a new one. Without it, such a trigger is ignored, and counted as `busy`. |
| `Late(d)` | How late the flow may run after it should have, 10 minutes by default. See [Running late](#running-late). |

For a light that should stay on while there's motion, a decision's `Linger` says it more simply.

## Steps

Everything that happens outside the flow, or varies between runs, is a step on its `FlowRun`:

| Step | Does |
|---|---|
| `r.Now()` | The time, instead of `time.Now` |
| `r.Sleep(d)` | Waits. A restart doesn't move the time it ends |
| `r.Get(Hallway.Occupied)` | Reads a `Bool`, a `Number` or a `Choice` now. A value nobody has reported yet ends the run as failed |
| `r.Is(cond)` | Whether a [condition](automations.md#conditions) is true now, such as `oikade.Daily(oikade.Sunset(0), oikade.Sunrise(0))` for whether it's dark. One that reads a value nobody has reported yet is false |
| `r.WaitFor(cond, timeout)` | Waits until a condition is true and returns true, or returns false at the timeout. Zero means no limit |
| `r.Set(Hallway.On.To(true))` | Writes a capability. On an arbitrated one it's a [claim](automations.md#claims-from-flows), which needs `.For(d)`, `.UntilReleased()` or `.UntilNext()`. `Hallway.On.To(true)` alone writes nothing, and fails `oikade check` |
| `r.Release(rule)` | Ends a rule's claims, and a case's latch: a flow's name, or a case's device's `Key`, capability and name, as `r.Release(Valve.Key + " on leak")` |
| `r.Do(name, call)` | Makes an outside call, a `func(context.Context) (T, error)`, and records and returns its result, or the zero value and its error |
| `r.Fail(err)` | Ends the run as failed, there and then, even if the flow recovers from it |

A `Set` or a `Release` that fails, such as on a device that doesn't answer or a rule your home doesn't have, is logged, and the run carries on and ends as failed. So a flow doesn't stop at a dead bulb, and `r.Fail` is for when it should stop.

## What a crash does

Each step is written to the host's disk before the flow continues. After a crash, your process starts again and runs the flow from the top. A step already recorded returns what it returned the first time, without happening again, until the flow reaches where it had got to and carries on.

So a flow has three rules:

- **Take the same steps in the same order.** Decide only from what steps return. Use an error's message rather than `errors.Is`, because a replayed error is a new one with the same message.
- **Make actions safe to repeat.** Actions run at least once, not exactly once: a crash after a write and before it's recorded repeats the write. Set a value, and never toggle one. `r.Do` is the same until its result is recorded, and once it is, the call never happens again.
- **Don't keep state outside the flow.** A variable is rebuilt by the replay. Anything else isn't.

`oikade check` rejects what would break the first rule inside a flow: `time.Now` and the rest of the `time` package's clock, any goroutine, and any `range` over a map, `maps.Keys`, `maps.Values` or `maps.All`. Range over `slices.Sorted(maps.Keys(m))` instead.

- **A flow is any function that takes a `*oikade.FlowRun`.** A function it calls without passing that along isn't checked, so a helper can read the clock.
- **What you pass to `r.Do` is exempt,** because that's where outside calls belong.

A trigger isn't a step. If your process dies after a trigger and before the run's first step reaches disk, that run never existed.

## Running late

A flow that's more than 10 minutes late gives up and logs it, rather than turn the lights off at 09:00 after an outage. `Late` sets a flow's own limit.

- **A run is late** if it wakes past the end of a wait, or comes back from a crash that long after its last step.
- **A time in `At` that passed** while no process was running still fires when one starts, within `Late`. After that it's skipped, logged and counted as `missed`. Only the latest missed time counts.
- **A flow a decision ranks above its override line** never gives up and always fires, however late, so a leak response still acts. Its `Set`s there are Safety claims, and pausing it needs `--allow-safety`. See [Claims from flows](automations.md#claims-from-flows).

## Changing a flow

A deploy or a restart replays a waiting flow against the code that's there now. Each step is compared with its record, including its arguments, and a run that no longer matches is dropped and logged. The next trigger starts it fresh.

Only steps up to where the run had got to can be compared. A change after that point just takes effect, and the run carries on with the new code.

## A flow's claims

A flow's claims belong to the flow, not to the run that made them, so a flow holding an `UntilReleased` claim should reach its `r.Release`. `r.Release` ends all of a flow's claims, whichever run made them, Safety ones included. If a run can't get there, this is what happens to its flow's claims:

- **A run that ends badly** releases all the flow's claims except Safety ones. Failing, panicking, giving up and being dropped all count. That includes claims left by an earlier run that succeeded.
- **Safety claims** end only at an `r.Release`, so a failed notification can't reopen a valve that a flow closed, or when a deploy that removes their rule settles. `oikade deploy` refuses that without `--allow-safety`.
- **A flow you remove** keeps its run and claims until a deploy without it is confirmed. Then all of them end, Safety ones included.
- **A run that was replaced** releases nothing. Its claims pass to the run that replaced it, and that run ending badly releases them.
- **A run that succeeds** releases nothing. A claim it didn't release stays until a later `r.Release`, or until a later run ends badly.
- **A run still ending when the process stops** releases nothing and keeps its record, and the next process finishes the job.

If a saved release is failing on a device, a later release that finds nothing left to change reports success, though the claim's end isn't on disk. It takes a save that keeps failing and then a host crash to bring that claim back.

## Limits

This isn't a workflow engine. There's no running old code to finish old runs, and no exactly-once. A run has at most 10,000 steps and 4 MiB of them, because a replay reads them all, and a result from `r.Do` is at most 1 MiB. A run past either limit fails. Something that should stay true is a decision, not a flow that loops forever.
