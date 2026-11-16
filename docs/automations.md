# Automations

How a home's rules are written: moments, holds, conditions, rooms, budgets and arbitration. The [README](../README.md#getting-started-in-go) gets you to a first deploy, and [writing-flows.md](writing-flows.md) covers flows.

## Automations

A home has two kinds of rule, plus [flows](writing-flows.md) for anything with a decision or a long wait in it ([OIK-64](https://linear.app/oikade/issue/OIK-64)):

- **Moments** do something when a trigger fires: a capability changes, or a time of day comes.
- **Holds** keep a capability at a value for as long as a window or a condition holds. The engine works out from the clock and the devices' state whether a hold is active, so it stays right across a restart.

Capabilities are last-write-wins unless they're **arbitrated** with a [`Control`](#arbitration). Then rules, Matter controllers and the device itself make claims, the device takes the winner, and `oikade status` says why.

### Moments

```go
oikade.NewMoment("hallway-motion-light", HallwayLight.On.To(true), oikade.Wait(5*time.Minute), HallwayLight.On.To(false)).
	Restart(). // new motion restarts the 5 minutes; without it, triggers are ignored while it runs
	On(HallwaySensor.Occupied.Becomes(true)).
	If(oikade.Daily("18:00", "07:00"))
```

`On` takes the changes that trigger it: a `Bool` or a `Number` fires whenever it changes, a `Bool`'s `Becomes(true)` only when it changes to true, and [`InRoom`](#rooms) on a change from any device in a room. `If` is a [condition](#conditions), looked at when a trigger fires.

The 5 minutes in this example live in your process's memory, so a restart during them leaves the light on. The [hold](#holds) version below doesn't have that problem, and a flow's `Sleep` survives it too.

### Conditions

A condition is something true or false about the home now. It's a value, so the host can say why one is false ([OIK-298](https://linear.app/oikade/issue/OIK-298)):

| Condition | True while |
|---|---|
| `GarageDoor.Open` | a `Bool` is true |
| `Bathroom.Humidity.Above(70)`, `.Below(3)`, `.Within(18, 24)` | a `Number` is above, below, or from the first to the second, both included |
| `oikade.Daily("22:00", "06:00")` | it's between two [times of day](#times-of-day), which may be a `Sunrise` or a `Sunset`. An end at or before the start is the next day. `.Days(time.Saturday, time.Sunday)` limits it to the windows that start on those days |
| `oikade.Yearly("1 Jun", "15 Sep")` | it's between two dates of the year, both whole days, in the host's time zone. An end before the start is the next year, so `Yearly("1 Nov", "28 Feb")` is the winter |
| `oikade.Any(a, b, …)`, `oikade.All(a, b, …)`, `oikade.Not(a)` | any of them is, all of them are, or it isn't |
| `oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool { … })` | the function says so, for what the others can't say |

```go
oikade.All(PoolWater.Temperature.Below(3), oikade.Not(oikade.Yearly("1 Jun", "15 Sep")))
```

A condition reading a value nobody has reported yet stops there: a hold keeps its state, and a moment doesn't run. If an `OnDelay` or `OffDelay` runs out meanwhile, the hold starts or ends once the value is back, if `When` still says so. The log says `Could not evaluate hold; keeping its state until it can` at Info when that starts, with the rule and why, and `Evaluated hold again` when it ends, once each however long it lasts.

A `ConditionFunc` reads the home through its `Snapshot`: `Get` reads a `Bool` or a `Number`, `Is` says whether another condition is true, `Any`, `All`, `Count` and `Readings` read a [room](#rooms), and `Now` is local time, in the host machine's zone. It must not block, because it holds up every other rule. Its label is how the host names it, since it can't see inside a function to say why it's false. `Lookup` carries on without a value, and reports whether there is one:

```go
oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool {
	v, ok := s.Lookup(CellarSensor.Humidity)
	return ok && v > 65
})
```

A date or time that isn't one, such as `Yearly("1 Jun", "31 Jun")`, `Within(25, 18)` or a `ConditionFunc` with no label, fails `oikade check`, naming the rule.

### Times of day

`At` fires at times of day, instead of or as well as `On`. `oikade.Clock("23:30")` is a local time, in the host machine's zone, from `"00:00"` to `"23:59"`:

```go
oikade.NewMoment("downstairs-bedtime", PorchLight.On.To(false)).
	At(oikade.Clock("23:30")).
	Late(10 * time.Minute)
```

`oikade.Sunrise` and `oikade.Sunset` are the day's sunrise and sunset, moved by an offset of up to 6 hours either way. They need the home's `Location`, made by `oikade.NewLocation` from its latitude and longitude in degrees (north and east are positive), which your process, `oikade dev`, `oikade replay` and `hometest` all read from the `Home`:

```go
oikade.NewHome(oikade.Options{Location: oikade.NewLocation(51.48, -0.01)}, // round to 2 decimals
	oikade.NewMoment("porch-at-dusk", PorchLight.On.To(true)).
		At(oikade.Sunset(-20*time.Minute)), // 20 minutes before sunset
)
```

Two decimals are about a kilometre, which moves sunset by seconds, so your home's repo needn't hold more than its postcode. A home with no sun times needs no `Location`. `NewLocation` takes both coordinates, so one can't be forgotten, and 0 is a coordinate like any other.

- **A time that isn't one,** such as `Clock("25:00")`, a sun time with no location, or an offset over 6 hours, fails `oikade check`, naming the rule.
- **A `Daily` takes two strings or two times,** so one with a sun edge writes its clock edge as `oikade.Clock("23:30")`.
- **Several times** fire at each, so `At(Clock("18:00"), Sunset(0))` fires at both, and the same time twice fires once.
- **A day the sun doesn't rise or set,** as near the poles, that time doesn't fire, and a window with it as an edge stays shut.
- **A `Daily` with a sun edge and a clock edge** ends at the first end after its start, as any `Daily` does. So in midsummer `Daily(Sunset(3*time.Hour), Clock("23:30"))` starts after midnight, and stays open until 23:30 that evening. Keep the offset small enough that the order holds all year.
- **A time the clocks skip** when daylight saving starts fires when they go forward, and a time that happens twice when it ends fires the first time.
- **`Late`** is how late a time may still fire after your process was down at it, 10 minutes by default. A later one is skipped, logged and counted as `missed`. A moment with a Safety `Set` fires however late it is. Only the latest missed time counts.

### Rooms

`oikade.InRoom` is a capability of a room's devices in a group, as a `Change`, a condition's read, or with `To` as a `Set` or a hold's `Set`. A device is in a room when the [registry](devices.md#devices) says so, so a lamp added to the room later is covered without a deploy, which is why a room takes a capability rather than handles.

```go
oikade.NewMoment("lights-out",
	oikade.InRoom("living-room", oikade.Lights, oikade.On).To(false), // the lights, not the plugs
).On(oikade.InRoom("hallway", oikade.Everything, oikade.Occupied)) // any sensor in the hallway
```

The group says which of the room's devices you mean, by their [device types](devices.md#what-a-device-is), because `on` is a light's, a plug's and a heater's alike, and "lights on at dusk" shouldn't turn on the heater ([OIK-255](https://linear.app/oikade/issue/OIK-255)):

| Group | Device types |
|---|---|
| `oikade.Lights` | `OnOffLight`, `DimmableLight` |
| `oikade.Plugs` | `OnOffPlugInUnit`, `DimmablePlugInUnit`, `MountedOnOffControl`, `MountedDimmableLoadControl` |
| `oikade.Heaters` | `Thermostat`: any device with a setpoint |
| `oikade.Kinds(...)` | the types you list, such as `oikade.Kinds(oikade.DimmableLight)` |
| `oikade.Everything` | every device in the room with the capability, whatever it is |

A device whose type isn't in the group is left alone, and the log names it, so a wall switch typed `MountedOnOffControl` is a plug until `oikade devices type` makes it a light. A room's sensors have no group of their own: read and trigger on them with `Everything`.

A `ConditionFunc` reads a room with its `Snapshot`'s `Any`, `All` and `Count` for a `bool` capability, and `Readings` for a number, whose values go to `slices.Max`, `slices.Min` or an average:

```go
// A dehumidifier that runs on whichever cellar sensors have reported.
oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool {
	return slices.Max(s.Readings(oikade.InRoom("cellar", oikade.Everything, oikade.Humidity))) > 65
})
```

A read covers the devices in the room now that have reported the capability, and ignores the rest, so one dead sensor doesn't stop a rule over three. If none has reported, the condition stops, as at a `Get`. A hold that reads a room looks again when a device moves in or out with `oikade devices room`, with no deploy. A sensor that reported and then died keeps its last value.

A room with no devices in it, or none in the read's group, stops the condition too, and a hold keeps its state: one that was on, or in its off-delay, stays on until a device is back in the room. The log says `Rule read a Room with no members` at Warn, once each time it happens, with the rule and the room. `oikade devices room`, `oikade devices type` and `oikade devices remove` still make the change when it leaves a room a hold reads with nothing of the read's group in it, but they warn on stderr, naming the room and the holds, and `--json` has the warning as `warning`.

- **Room triggers** fire on a change from any device in the room and the group at the time of the change.
- **Room actions** set every device in the room and the group that has reported the capability, in name order, and skip the rest. So `on` on a room's `Everything` leaves the motion sensor alone, and turns off a heater along with the lamps, while `Lights` leave the heaters alone ([OIK-20](https://linear.app/oikade/issue/OIK-20)). Membership is read when the action runs, so a lamp added to the room during a `Wait` is included in the actions after it.
- **A room hold takes up new members while it's active.** A device that moves into the room with `oikade devices room`, or is retyped into the hold's group, is written, or claimed at the hold's level, at once, and the log says `Held Room member that joined`. A member that's discovered after the hold started is written then. One that leaves the room lets go of the hold's claim, and is written again if it comes back. After a restart, the hold counts the room's members as held, discovered yet or not, and doesn't write them again.
- **If some devices fail**, the others are still set and the remaining actions still run, so a dead bulb can't leave the rest of the room on. The run counts as `failed`. A room with nothing to set counts as `succeeded`.
- **`Restart` with a room trigger** restarts the run on a change from any sensor in the room, which suits people moving between sensors. Drivers only report real changes, so someone standing still in front of one sensor doesn't restart the run. Keep the `Wait` longer than the time your sensor holds occupancy after the last movement.
- **A room `Set` with no end skips arbitrated members,** because a claim must end (see [one-off claims](#one-off-claims)). The log names them, with members of other types, as `Skipped Room members`.

### Holds

`oikade.NewHold` takes a name and the `Set` to keep. A hold keeps that value while `When` is true:

```go
oikade.NewBundle(
	oikade.NewHold("water-heater-night", WaterHeater.On.To(true)).
		When(oikade.Daily("22:00", "06:00")), // an end at or before the start is the next morning
	oikade.NewHold("bathroom-humid", BathroomFan.On.To(true)).
		When(BathroomSensor.Humidity.Above(70)).
		OffDelay(10*time.Minute), // run on for 10 minutes after it drops
)
```

| Method | |
|---|---|
| `When` | A [condition](#conditions). |
| `OnDelay` | How long `When` must stay true before the hold starts. |
| `OffDelay` | How long the hold stays active after `When` stops being true. |
| `Latch` | Stays active once it starts, until a `Release`. See [below](#latched-safety-holds). |
| `Budget` | Runs a device for a number of minutes a day instead, in the slots that suit it best. It's the only condition of its hold, and a lock can't have one. See [Budgets](#budgets). |

A hold needs a `When` or a `Budget`.

A `When` is looked at again on every device change and at least once a minute. A `Daily` in it, on its own or in an `All`, is different: the hold starts and ends on its edge, and `oikade status` shows when it next does. One inside an `Any` or a `Not` changes within a minute of its edge.

- **When a hold starts,** your process writes its value. When it ends, a bool hold writes the opposite, and a number hold writes nothing, so the device keeps the held value. A `Locked` hold writes nothing either: only `To(false)` unlocks a door, so `BackDoor.Locked.To(true)` over a night's `Daily` leaves it locked in the morning, and a hold of `To(false)` leaves it unlocked when it ends ([OIK-23](https://linear.app/oikade/issue/OIK-23)). A lock with a `Control` is left at the `Control`'s default when a claim on it ends, as any arbitrated capability is, so give it `Locked.To(true)` as its default unless it should unlock by itself. To go back to a set value, give the capability a `Control`: its default is what's left when the hold ends.
- **Without a `Control`, the last write wins.** A Home app tap at 23:00 turns the water heater off until the hold next starts.
- **Restarts are safe.** The host saves each hold's state, and a new process writes only if the hold started or ended meanwhile. A restart at 02:00 doesn't undo the 23:00 tap, but automations down from 21:50 to 02:00 turn the heater on when they come back. `OffDelay` deadlines are saved too, so the fan still stops on time.
- **Two holds on one capability need a `Control`** to decide between them.

### Budgets

A budget hold runs a device for a number of minutes a day, and plans when. Here a loft fan runs one to five hours a day depending on how humid the loft was at midnight, overnight where it can, and never in the evening:

```go
oikade.NewBundle(
	oikade.NewControl(LoftFan.On.To(false), oikade.PreferOn, oikade.ExternalOverride).
		OverrideHold(time.Hour).
		MinimumOn(time.Hour).        // each run at least an hour
		MinimumOff(30*time.Minute). // and half an hour's rest between runs
		MaximumStartsPerDay(3),
	oikade.NewHold("loft-airing", LoftFan.On.To(true)).
		Budget(oikade.NewBandedBudget(oikade.Bands{
			Reading: LoftHumidity.Humidity,
			MaxAge:  time.Hour, Missing: 180, // if the sensor hasn't reported in the last hour
			Ranges:    []oikade.Band{{Below: 55, Minutes: 60}, {Below: 70, Minutes: 180}},
			Otherwise: 300,
		}).
			Prefer(oikade.Daily("23:00", "07:00"), 3).
			Forbid(oikade.Daily("18:00", "22:00"))),
)
```

`oikade.NewBudget` takes a function of the home, and `oikade.NewBandedBudget` takes `oikade.Bands`. The rest are chained:

| Method or input | |
|---|---|
| `PlanningTime` | When each planning day starts, `"HH:MM"` local time. The day is planned then, and the plan is frozen until the next one. Defaults to `"00:00"`. |
| `SlotMinutes` | The day is planned in slots of 15, 30 or 60 minutes. Defaults to 30. |
| `oikade.Bands` | The day's minutes from the band the reading falls in at `PlanningTime`: the first whose `Below` it's under, and `Otherwise` for the rest. `Missing` applies when it hasn't reported within `MaxAge` (default 30 minutes). |
| `NewBudget`'s function | Or a function of the home at planning time. If it panics or reads a device that hasn't reported, the day's budget is zero, and the log says why. |
| `Prefer(hours, score)` | A `Daily` the minutes go in first. Each slot it touches gains the score, from -100 to 100, and overlapping ones add up. Give it once per `Daily`. |
| `Forbid` | `Daily`s the minutes never go in. |

A budget's `Daily`s take clock times only, because the day is planned in slots.

A budget holds one device's `Bool` capability at `true`, and that capability needs a `Control`: its `MinimumOn`, `MinimumOff` and `MaximumStartsPerDay` shape the planned runs, and `MinimumOn` and `MinimumOff` must be whole numbers of slots. `oikade check` says if there's no `Control`. Something that must run whatever the budget says, such as frost protection, is a separate `Safety` hold on the same capability, which wins over the budget.

At `PlanningTime` your process plans the day and freezes the plan. The hold is active in the planned blocks, as a Normal claim, so a Safety hold or a Home app tap still beats it. Run minutes count only while the device reports on and is available. The plan and run minutes are saved in the host, so a deploy in the middle of the day doesn't run it again. Editing a budget takes effect at the next `PlanningTime`. Automations that start with no plan for today plan from then, so the hours already gone are lost to the day.

### Arbitration

A capability is arbitrated when your home has a `Control` for it. The host saves that, so a device is arbitrated, and a Safety claim holds, while your automations are down.

```go
oikade.NewBundle(
	oikade.NewControl(
		HallLight.On.To(false),     // what's left when nobody claims it
		oikade.PreferOn,         // two claims at one level: on wins
		oikade.ExternalOverride, // a wall switch or Home app tap becomes a claim...
	).OverrideUntilNext(), // ...until what the rules want next changes
	oikade.NewHold("hall-motion-light", HallLight.On.To(true)).
		When(HallMotion.Occupied).
		OffDelay(5*time.Minute),
)
```

Rules, Matter controllers and the device itself make **claims**. The highest level with a claim wins:

| Level | Who claims | Ends |
|---|---|---|
| Safety | Rules with `Priority(oikade.Safety)` | A rule's claim ends when its hold ends (for a latched hold, at a `Release`), or as its `Set` says |
| Override | Outside changes (a Matter controller, `oikade set`, or the device by itself), and rules with `Priority(oikade.Override)` | An outside change ends after `OverrideHold`. A rule's claim ends as above. |
| Normal | Ordinary rules | As above |
| Default | Nobody | Never |

`oikade.NewControl` takes the first three, in order. The rest are chained:

| Input or method | |
|---|---|
| Default | Required. The capability, at its value when nothing claims it: a handle's `To`. |
| Resolve | Required. Which claim wins within a level: `PreferOn` or `PreferOff` for bools, `Highest` or `Lowest` for numbers. Rules have no numeric priorities. |
| External | Required. What an outside change does. `ExternalOverride` makes it a claim for `OverrideHold`. `ExternalRevert` writes the winner straight back, as for a valve that must stay closed. |
| `OverrideHold`, `OverrideUntilNext` | With `ExternalOverride`: a duration, or until the winner without it next changes. |
| `MinimumOn`, `MaximumOn`, `MinimumOff`, `MaximumStartsPerDay` | Equipment limits. They apply after the winner is picked and beat every rule, Safety included. An outside change is accepted and counted, but not fought. |

In this example, motion turns the light on, and it goes off 5 minutes after the motion stops. A wall switch turned off during that is an Override, so the light stays off until the motion hold ends. A wall switch turned on with nobody about lasts until the next motion starts, and the light then goes off 5 minutes after the motion stops. The hold's off-delay is saved, so a restart doesn't leave the light on.

A report soon after a write is the device catching up, not an outside change: for 5 seconds, or as long as the capability takes to settle, which is 2 minutes for a blind's `position` and 30 seconds for `locked` ([OIK-23](https://linear.app/oikade/issue/OIK-23)). So a blind reporting 20, 40 and 60 on its way to 70 isn't an Override. A report of something else once that's up is, so a blind that someone stopped at 45 is an Override at 45, two minutes after the write.

An outside change is recorded by the path it came in by: `Matter` for a command through the bridge, such as a Home app tap, `Command` for `oikade set`, and `Device` for anything else, such as a wall switch. A rule's claim is `Rule/<name>`. Oikade can't tell what a person did, so nothing is called "manual".

### One-off claims

A `Set` on an arbitrated capability is a claim, so it must say when it ends:

```go
oikade.NewMoment("towel-rail-boost", TowelRail.On.To(true).For(time.Hour).UntilNext()).
	At(oikade.Clock("21:30"))
```

- `For` ends it after that long.
- `UntilNext` ends it when the winner without it next changes. If the rail also had a hold from 06:00 to 08:00, that's when the hold starts the next morning.
- `For` together with `UntilNext` ends it at whichever comes first. This boost ends after an hour, unless what the other claims want changes sooner.
- `UntilReleased` ends it at an `oikade.Release("<name>")` action in another rule.
- `Priority(oikade.Override)` on the `Set` raises the claim's level. It's Normal otherwise, or in a flow the flow's `Priority`.

A `Set` with none of these on an arbitrated capability of a single device is an error when it runs. A `Set` on a capability that isn't arbitrated is a plain write, and these don't apply to it.

### Latched Safety holds

A latched hold stays active once it has started, until another rule releases it. It's worked out from state rather than from a change. So if the garage overheated while your process was down and is still too hot when it next sees the temperature, it latches then.

```go
oikade.NewBundle(
	oikade.NewControl(GarageHeater.On.To(true), oikade.PreferOff, oikade.ExternalRevert),
	oikade.NewHold("garage-overheat", GarageHeater.On.To(false)).
		Priority(oikade.Safety).
		Latch().
		When(GarageTemperature.Temperature.Above(40)),
	oikade.NewMoment("garage-overheat-reset", oikade.Release("garage-overheat")).
		On(GarageDoor.Open.Becomes(true)),
)
```

Overheating turns the heater off. Turning it on from the Home app is reverted at once, and the heater stays off after the garage cools, until someone opens the door to check. The Home app keeps showing your tap for about 10 seconds before it shows the heater off again. Oikade has already sent the real value by then, and sending it later doesn't change the wait ([OIK-239](https://linear.app/oikade/issue/OIK-239)). Rules run only while your process is up. If the garage overheats and cools again while it's down, nothing latches. A hold's latch lives under its name, so don't rename it: a deploy refuses that without `--allow-safety-removal`.

### Composing a home

A home is made of parts. Each `Hold`, `Moment`, `Flow`, `Control` and `Plugin` is an `oikade.Part`, and `oikade.NewHome` takes any number of them after its `oikade.Options`. An ordinary Go function can return one, so what belongs together stays together and can move between files. `oikade.NewBundle` groups parts, and is a part itself, so bundles nest:

```go
func home() oikade.Home {
	return oikade.NewHome(oikade.Options{Location: oikade.NewLocation(51.48, -0.01)},
		lights(),
		dehumidifier(),
	)
}

func lights() oikade.Bundle {
	var parts []oikade.Part
	for _, z := range zones {
		parts = append(parts, motionLight(z))
	}
	return oikade.NewBundle(parts...)
}

func motionLight(z zone) oikade.Hold {
	return oikade.NewHold(z.name+"-motion-light", z.light.To(true)).
		When(z.motion).
		OffDelay(5 * time.Minute)
}

// A budgeted hold and the Control it needs.
func dehumidifier() oikade.Bundle {
	return oikade.NewBundle(
		oikade.NewControl(Dehumidifier.On.To(false), oikade.PreferOn, oikade.ExternalOverride).
			OverrideHold(time.Hour),
		oikade.NewHold("dehumidifier", Dehumidifier.On.To(true)).Budget(cellarBudget),
	)
}
```

A helper can be tested on its own: `hometest.New(t, oikade.NewHome(oikade.Options{}, dehumidifier()), hometest.Options{})`.

- **A rule keeps the name you wrote,** whichever `Bundle` it's in, because a name is the rule's state key. A helper used twice builds a different name each time, and a clash fails `oikade check`.
- **`Options` holds what a home has exactly one of,** so a second location, or one inside a helper, doesn't compile. A home with no sunrise or sunset writes `oikade.Options{}`.
- **A nil part, or a pointer to one,** fails `oikade check`, which says where it is.

### Reading status

`oikade status` shows each capability, and for an arbitrated one, the claim that won:

```console
$ oikade status hall-light
hall-light  on  false  Device wants false at Override
```

`oikade status --json` lists every claim, with `expires` when it ends at a set time, `latched` for a latched hold, and `reverted` for an outside change that `ExternalRevert` wrote back. `oikade_device_control_level` has the winning level over time.
