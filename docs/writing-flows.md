# Writing flows

A flow is an automation written as ordinary Go, with real `if`, `for` and `switch`. It can wait for hours, and it survives a crash at any point. Use a [decision](automations.md#decisions) when something should stay true, and a flow when something should happen.

This belongs to [the host](architecture.md#the-host). `hometest` tests a flow on a fake clock, crashes included: [Getting started in Go](../README.md#getting-started-in-go).

```go
func bedtime(r *oikade.FlowRun) {
	r.Set(oikade.InRoom("downstairs", oikade.Lights, oikade.Brightness).To(20))
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
	home.Flow("bedtime", bedtime).At(oikade.Clock("22:30"))
	home.Run()
}
```

`home.Flow` adds a flow, named once. `When` and `At` say what starts it, `If` when it may, and `Restart` what a trigger does while it runs: [Flows' triggers](automations.md#flows-triggers).

## Steps

Everything that happens outside the flow, or varies between runs, is a step on its `FlowRun`:

| Step | Does |
|---|---|
| `r.Now()` | The time, instead of `time.Now` |
| `r.Sleep(d)` | Waits. A restart doesn't move the time it ends |
| `r.Get(Hallway.Occupied)` | Reads a `Bool` or a `Number` now. A value nobody has reported yet ends the run as failed |
| `r.Is(cond)` | Whether a [condition](automations.md#conditions) is true now, such as `oikade.Daily(oikade.Sunset(0), oikade.Sunrise(0))` for whether it's dark. One that reads a value nobody has reported yet is false |
| `r.WaitFor(cond, timeout)` | Waits until a condition is true and returns true, or returns false at the timeout. Zero means no limit |
| `r.Set(Hallway.On.To(true))` | Writes a capability. On an arbitrated one it's a claim, which needs `.For(d)`, `.UntilReleased()` or `.UntilNext()`. `Hallway.On.To(true)` alone writes nothing, and fails `oikade check` |
| `r.Release(rule)` | Ends a rule's claims, and a case's latch |
| `r.Do(name, call)` | Makes an outside call, a `func(context.Context) (T, error)`, and records and returns its result and error |
| `r.Fail(err)` | Ends the run as failed, there and then |

A `Set` or a `Release` that fails, such as on a device that doesn't answer, is logged, and the run carries on and ends as failed. So a flow doesn't stop at a dead bulb, and `r.Fail` is for when it should stop.

## What a crash does

Each step is written to the host's disk before the flow continues. After a crash, your process starts again and runs the flow from the top. A step already recorded returns what it returned the first time, without happening again, until the flow reaches where it had got to and carries on.

So a flow has three rules:

- **Take the same steps in the same order.** Decide only from what steps return, and from an error's message rather than `errors.Is`, because a replayed error is a new one with the same message. `oikade check` rejects `time.Now`, the rest of the `time` package's clock, any goroutine and any `range` over a map, `maps.Keys`, `maps.Values` or `maps.All` inside a flow. Range over `slices.Sorted(maps.Keys(m))` instead. `internal/flowcheck` is that check. A function counts as a flow if it takes a `*oikade.FlowRun`. What you pass to `r.Do` is exempt, because that's where outside calls belong.
- **Make actions safe to repeat.** Actions run at least once, not exactly once. A crash after a write and before its journal entry repeats the write. Set a value, and never toggle one. `r.Do` is the same until its result is recorded, and once it is, the call never happens again.
- **Don't keep state outside the flow.** A variable is rebuilt by the replay. Anything else isn't.

A trigger isn't a step. If your process dies after a trigger and before the run's first entry reaches disk, that run never existed.

## Running late

A flow that's more than 10 minutes late gives up and logs it, rather than turn the lights off at 09:00 after an outage. `Late` sets a flow's own limit. A run is late if it wakes past the end of a wait, or comes back from a crash that long after its last step. A flow's `Set` on a decided capability ranks at the decision's [`Flows` line](automations.md#claims-from-flows) for it, and above the override line it's a Safety claim. A flow any decision ranks there never gives up late, so a leak response still acts, and pausing it needs `--allow-safety`. A `Set` with no `Flows` line to rank at fails the run.

Times of day follow the same rule. A flow's time in `At` that passed while no process was running still fires when one starts, within `Late`, and is skipped and logged after that. Only the last missed time counts. A flow a decision ranks above its override line always fires.

## Changing a flow

A deploy or a restart replays a waiting flow against the code that's there now. Each step is compared with its journal entry, including its arguments, and a run that no longer matches is dropped and logged. The next trigger starts it fresh.

Only steps up to where the run had got to can be compared. A change after that point just takes effect, and the run carries on with the new code.

A flow's claims belong to the flow, not to the run that made them, so a flow holding an `UntilReleased` claim should reach its `r.Release`. If it can't, this is what happens to them:

- **A run that ends badly** releases all the flow's claims except Safety ones. Failing, panicking, giving up and being dropped all count. A flow you remove is different: its run and claims stay until a deploy without it is confirmed, then all of them end, Safety ones included, past the check that refuses that. That includes claims left by an earlier run that succeeded: a later failing run ending them is intended.
- **Safety claims** end only by an explicit `r.Release`, so a failed notification can't reopen a valve that a flow closed, or when a deploy that removes their rule settles. `oikade deploy` refuses that unless told to go ahead.
- **A run that was replaced** releases nothing. Its claims pass to the run that replaced it, and that run ending badly releases them.
- **A run that succeeds** releases nothing. A claim it didn't release stays until a later `r.Release`, or until a later run ends badly.
- **A run still ending when the process stops** releases nothing and keeps its journal, and the next process finishes the job.

`r.Release` ends all of a flow's claims, whichever run made them, Safety ones included.

If a saved release is failing on a device, a later release that finds nothing left to change reports success, though the claim's end isn't on disk. It takes a save that keeps failing and then a host crash to bring that claim back.

Starting a run while one is in progress is ignored. With `Restart()`, the new trigger cancels the run in progress instead, and its journal goes with it.

## Limits

This isn't a workflow engine. There's no running old code to finish old runs, and no exactly-once. A run has at most 10,000 steps, because a replay reads them all, and a result from `r.Do` is at most 1 MiB. Something that should stay true is a decision, not a flow that loops forever.
