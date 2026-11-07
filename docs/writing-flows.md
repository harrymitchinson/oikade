# Writing flows

A flow is an automation written as ordinary Go, with real `if`, `for` and `switch`. It can wait for hours, and it survives a crash at any point. Use a hold when something should stay true, a moment when a fixed list of actions will do, and a flow for anything with a decision or a long wait in it.

This belongs to [the host](architecture.md#the-host). `hometest` tests a flow on a fake clock, crashes included: [Getting started in Go](../README.md#getting-started-in-go).

```go
func bedtime(r *framework.FlowRun) error {
	if err := r.Set(framework.InRoom("downstairs", framework.Brightness).To(20)); err != nil {
		return err
	}
	r.Sleep(30 * time.Minute)

	var late bool
	if err := r.Do("calendar", &late, func(ctx context.Context) error {
		var err error
		late, err = calendar.HasGuests(ctx)
		return err
	}); err != nil {
		return err
	}
	if late {
		r.Sleep(time.Hour)
	}
	return r.Set(LoungeLamp.On.To(false))
}

func main() {
	framework.Run(framework.Home{
		Flows: []framework.Flow{{Name: "bedtime", At: []framework.Time{framework.Clock("22:30")}, Run: bedtime}},
	})
}
```

## Steps

Everything that happens outside the flow, or varies between runs, is a step on its `FlowRun`:

| Step | Does |
|---|---|
| `r.Now()` | The time, instead of `time.Now` |
| `r.Sleep(d)` | Waits. A restart doesn't move the time it ends |
| `r.WaitFor(cond, timeout)` | Waits until `cond` is true and returns true, or returns false at the timeout. Zero means no limit |
| `r.Set(Hallway.On.To(true))` | Writes a capability. On an arbitrated one it's a claim, which needs `.For(d)`, `.UntilReleased()` or `.UntilNext()` |
| `r.Release(rule)` | Ends a rule's claims, and a hold's latch |
| `r.Do(name, &result, call)` | Makes an outside call, and records its result and error |

## What a crash does

Each step is written to the host's disk before the flow continues. After a crash, your process starts again and runs the flow from the top. A step already recorded returns what it returned the first time, without happening again, until the flow reaches where it had got to and carries on.

So a flow has three rules:

- **Take the same steps in the same order.** Decide only from what steps return, and from an error's message rather than `errors.Is`, because a replayed error is a new one with the same message. `oikade check` rejects `time.Now`, the rest of the `time` package's clock, any goroutine and any `range` over a map inside a flow. `internal/flowcheck` is that check. A function counts as a flow if it takes a `*framework.FlowRun`. What you pass to `r.Do` is exempt, because that's where outside calls belong.
- **Make actions safe to repeat.** Actions run at least once, not exactly once. A crash after a write and before its journal entry repeats the write. Set a value, and never toggle one. `r.Do` is the same until its result is recorded, and once it is, the call never happens again.
- **Don't keep state outside the flow.** A variable is rebuilt by the replay. Anything else isn't.

A trigger isn't a step. If your process dies after a trigger and before the run's first entry reaches disk, that run never existed, as with a moment.

## Running late

A flow that's more than 10 minutes late gives up and logs it, rather than turn the lights off at 09:00 after an outage. `Late` sets a flow's own limit. A run is late if it wakes past the end of a wait, or comes back from a crash that long after its last step. A flow with `Priority: framework.Safety` never expires, so a leak response still acts late, and its `Set`s default to that priority.

Times of day follow the same rule. A moment or flow's time in `At` that passed while no process was running still fires when one starts, within `Late`, and is skipped and logged after that. Only the last missed time counts. A moment with a Safety `Set` always fires.

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

Starting a run while one is in progress is ignored. With `Restart: true`, the new trigger cancels the run in progress instead, and its journal goes with it.

## Limits

This isn't a workflow engine. There's no running old code to finish old runs, and no exactly-once. A run has at most 10,000 steps, because a replay reads them all, and a result from `r.Do` is at most 1 MiB. A flow that loops forever is a hold or a moment.
