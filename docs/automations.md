# Automations

How a home's rules are written: moments, holds, rooms, budgets and arbitration. The [README](../README.md#getting-started-in-go) gets you to a first deploy, and [writing-flows.md](writing-flows.md) covers flows.

## Automations

A home has two kinds of rule, plus [flows](writing-flows.md) for anything with a decision or a long wait in it ([OIK-64](https://linear.app/oikade/issue/OIK-64)):

- **Moments** do something when a trigger fires: a capability changes, or a time of day comes.
- **Holds** keep a capability at a value for as long as a window or a condition holds. The engine works out from the clock and the devices' state whether a hold is active, so it stays right across a restart.

Capabilities are last-write-wins unless they're **arbitrated** with a [`Control`](#arbitration). Then rules, Matter controllers and the device itself make claims, the device takes the winner, and `oikade status` says why.

### Moments

```go
framework.NewMoment("hallway-motion-light", HallwayLight.On.To(true), framework.Wait(5*time.Minute), HallwayLight.On.To(false)).
	Restart(). // new motion restarts the 5 minutes; without it, triggers are ignored while it runs
	On(framework.InRoom("hallway", framework.Occupied)).
	If(func(s framework.State) bool {
		return s.Any(framework.InRoom("hallway", framework.Occupied)) &&
			s.Within(framework.NewWindow(framework.Clock("18:00"), framework.Clock("07:00")))
	})
```

A condition reads the home through its `State`: `Get` reads a handle, `Any`, `All`, `Count` and `Readings` read a [room](#rooms), `Within` says whether it's inside a [window](#holds), by the rules a hold's `Window` keeps, and `Now` is local time, in the host machine's zone. It must not block, because it holds up every other rule. A condition reading a value nobody has reported yet stops there: a hold keeps its state, and a moment doesn't run. If an `OnDelay` or `OffDelay` runs out meanwhile, the hold starts or ends once the value is back, if `When` still says so. `Lookup` carries on instead, and reports whether there's a value:

```go
if v, ok := s.Lookup(CellarSensor.Humidity); ok && v > 65 {
```

The 5 minutes in this example live in your process's memory, so a restart during them leaves the light on. The [hold](#holds) version below doesn't have that problem, and a flow's `Sleep` survives it too.

### Times of day

`At` fires at times of day, instead of or as well as `On`. `framework.Clock("23:30")` is a local time, in the host machine's zone, from `"00:00"` to `"23:59"`:

```go
framework.NewMoment("downstairs-bedtime", PorchLight.On.To(false)).
	At(framework.Clock("23:30")).
	Late(10 * time.Minute)
```

`framework.Sunrise` and `framework.Sunset` are the day's sunrise and sunset, moved by an offset of up to 6 hours either way. They need the home's `Location`, made by `framework.NewLocation` from its latitude and longitude in degrees (north and east are positive), which your process, `oikade dev`, `oikade replay` and `hometest` all read from the `Home`:

```go
framework.NewHome(framework.Options{Location: framework.NewLocation(51.48, -0.01)}, // round to 2 decimals
	framework.NewMoment("porch-at-dusk", PorchLight.On.To(true)).
		At(framework.Sunset(-20*time.Minute)), // 20 minutes before sunset
)
```

Two decimals are about a kilometre, which moves sunset by seconds, so your home's repo needn't hold more than its postcode. A home with no sun times needs no `Location`. `NewLocation` takes both coordinates, so one can't be forgotten, and 0 is a coordinate like any other.

- **A time that isn't one,** such as `Clock("25:00")`, a sun time with no location, or an offset over 6 hours, fails `oikade check`, naming the rule.
- **Several times** fire at each, so `At(Clock("18:00"), Sunset(0))` fires at both, and the same time twice fires once.
- **A day the sun doesn't rise or set,** as near the poles, that time doesn't fire, and a window with it as an edge stays shut.
- **A window with a sun edge and a clock edge** ends at the first end after its start, as any window does. So in midsummer `NewWindow(Sunset(3 * time.Hour), Clock("23:30"))` starts after midnight, and stays open until 23:30 that evening. Keep the offset small enough that the order holds all year.
- **A time the clocks skip** when daylight saving starts fires when they go forward, and a time that happens twice when it ends fires the first time.
- **`Late`** is how late a time may still fire after your process was down at it, 10 minutes by default. A later one is skipped, logged and counted as `missed`. A moment with a Safety `Set` fires however late it is. Only the latest missed time counts.

### Rooms

`framework.InRoom` is a capability of every device in a room, as a `Change`, a condition's read, or with `To` as a `Set` or a hold's `Set`. A device is in a room when the [registry](devices.md#devices) says so, so a lamp added to the room later is covered without a deploy, which is why a room takes a capability rather than handles.

```go
framework.NewMoment("lights-out",
	framework.InRoom("living-room", framework.On).To(false), // everything there that has on
).On(framework.InRoom("hallway", framework.Occupied)) // any sensor in the hallway
```

A room without types means every device in it with the capability, and `on` is a light's, a switch's and an outlet's alike, so that turns off the fridge's plug too. `Types` limits a room write, a moment's or a hold's, to devices of some [device types](devices.md#what-a-device-is):

```go
framework.InRoom("living-room", framework.On).Types(framework.OnOffLight, framework.DimmableLight).To(false) // the lights, not the plugs
```

A device whose type isn't listed is left alone, and the log names it, so a new kind of light is left alone until you add its type. A room read takes `Types` too, but a room trigger doesn't: it fires for any device in the room.

A condition reads a room with `State`'s `Any`, `All` and `Count` for a `bool` capability, and `Readings` for a number, whose values go to `slices.Max`, `slices.Min` or an average:

```go
// A dehumidifier that runs on whichever cellar sensors have reported.
When: func(s framework.State) bool {
	return slices.Max(s.Readings(framework.InRoom("cellar", framework.Humidity))) > 65
},
```

A read covers the devices in the room now that have reported the capability, and ignores the rest, so one dead sensor doesn't stop a rule over three. If none has reported, the condition stops, as at a `Get`. A hold that reads a room looks again when a device moves in or out with `oikade devices room`, with no deploy. A sensor that reported and then died keeps its last value.

A room with no devices in it, or none of the read's `Types`, stops the condition too, and a hold keeps its state: one that was on, or in its off-delay, stays on until a device is back in the room. The log says `Rule read a Room with no members` at Warn, once each time it happens, with the rule and the room. `oikade devices room` still moves the device when it leaves a room a hold reads with nothing in it, but it warns on stderr, naming the holds, and `--json` has the warning as `warning`.

- **Room triggers** fire on a change from any device in the room at the time of the change.
- **Room actions** set every device in the room that has reported the capability, and is one of the `Types` if there are any, in name order, and skip the rest. So `on` on a room leaves the motion sensor and the thermostat alone. Membership is read when the action runs, so a lamp added to the room during a `Wait` is included in the actions after it.
- **A room hold takes up new members while it's active.** A device that moves into the room with `oikade devices room`, or is retyped into the hold's `Types`, is written, or claimed at the hold's level, at once, and the log says `Held Room member that joined`. A member that's discovered after the hold started is written then. One that leaves the room lets go of the hold's claim, and is written again if it comes back. After a restart, the hold counts the room's members as held, discovered yet or not, and doesn't write them again.
- **If some devices fail**, the others are still set and the remaining actions still run, so a dead bulb can't leave the rest of the room on. The run counts as `failed`. A room with nothing to set counts as `succeeded`.
- **`Restart` with a room trigger** restarts the run on a change from any sensor in the room, which suits people moving between sensors. Drivers only report real changes, so someone standing still in front of one sensor doesn't restart the run. Keep the `Wait` longer than the time your sensor holds occupancy after the last movement.
- **A room `Set` with no end skips arbitrated members,** because a claim must end (see [one-off claims](#one-off-claims)). The log names them, with members of other types, as `Skipped Room members`.

### Holds

`framework.NewHold` takes a name and the `Set` to keep. A hold keeps that value while its `Window` is open and `When` is true:

```go
framework.NewBundle(
	framework.NewHold("water-heater-night", WaterHeater.On.To(true)).
		Window(framework.NewWindow(framework.Clock("22:00"), framework.Clock("06:00"))), // an end at or before the start is the next morning
	framework.NewHold("bathroom-humid", BathroomFan.On.To(true)).
		When(func(s framework.State) bool {
			return s.Get(BathroomSensor.Humidity) > 70
		}).
		OffDelay(10*time.Minute), // run on for 10 minutes after it drops
)
```

| Method | |
|---|---|
| `Window` | `framework.NewWindow(start, end)`, from times of day such as `framework.Clock("22:00")` or `framework.Sunset(0)`, as in [`At`](#times-of-day). `.Days(…)` on it limits it to windows that start on those days. |
| `When` | A condition, as in a moment. |
| `OnDelay` | How long `When` must stay true before the hold starts. |
| `OffDelay` | How long the hold stays active after `When` stops being true. |
| `Latch` | Stays active once it starts, until a `Release`. See [below](#latched-safety-holds). |
| `Budget` | Runs a device for a number of minutes a day instead, in the slots that suit it best. It's the only condition of its hold. See [Budgets](#budgets). |

A hold needs a `Window`, a `When` or a `Budget`, and everything in it must be true together.

A `When` is looked at again on every device change and at least once a minute, so one that uses `s.Within` changes within a minute of the window's edge. A hold's own `Window` changes on the edge, and `oikade status` shows when it next does, so prefer it when the window is the whole story.

- **When a hold starts,** your process writes its value. When it ends, a bool hold writes the opposite, and a number hold writes nothing, so the device keeps the held value. To go back to a set value, give the capability a `Control`: its default is what's left when the hold ends.
- **Without a `Control`, the last write wins.** A Home app tap at 23:00 turns the water heater off until the hold next starts.
- **Restarts are safe.** The host saves each hold's state, and a new process writes only if the hold started or ended meanwhile. A restart at 02:00 doesn't undo the 23:00 tap, but automations down from 21:50 to 02:00 turn the heater on when they come back. `OffDelay` deadlines are saved too, so the fan still stops on time.
- **Two holds on one capability need a `Control`** to decide between them.

### Budgets

A budget hold runs a device for a number of minutes a day, and plans when. Here a loft fan runs one to five hours a day depending on how humid the loft was at midnight, overnight where it can, and never in the evening:

```go
framework.NewBundle(
	framework.NewControl(LoftFan.On.To(false), framework.PreferOn, framework.ExternalOverride).
		OverrideHold(time.Hour).
		MinimumOn(time.Hour).        // each run at least an hour
		MinimumOff(30*time.Minute). // and half an hour's rest between runs
		MaximumStartsPerDay(3),
	framework.NewHold("loft-airing", LoftFan.On.To(true)).
		Budget(framework.NewBandedBudget(framework.Bands{
			Reading: LoftHumidity.Humidity,
			MaxAge:  time.Hour, Missing: 180, // if the sensor hasn't reported in the last hour
			Ranges:    []framework.Band{{Below: 55, Minutes: 60}, {Below: 70, Minutes: 180}},
			Otherwise: 300,
		}).
			Prefer(framework.NewWindow(framework.Clock("23:00"), framework.Clock("07:00")), 3).
			Forbid(framework.NewWindow(framework.Clock("18:00"), framework.Clock("22:00")))),
)
```

`framework.NewBudget` takes a function of the home, and `framework.NewBandedBudget` takes `framework.Bands`. The rest are chained:

| Method or input | |
|---|---|
| `PlanningTime` | When each planning day starts, `"HH:MM"` local time. The day is planned then, and the plan is frozen until the next one. Defaults to `"00:00"`. |
| `SlotMinutes` | The day is planned in slots of 15, 30 or 60 minutes. Defaults to 30. |
| `framework.Bands` | The day's minutes from the band the reading falls in at `PlanningTime`: the first whose `Below` it's under, and `Otherwise` for the rest. `Missing` applies when it hasn't reported within `MaxAge` (default 30 minutes). |
| `NewBudget`'s function | Or a function of the home at planning time. If it panics or reads a device that hasn't reported, the day's budget is zero, and the log says why. |
| `Prefer(window, score)` | Windows the minutes go in first. Each slot a window touches gains the score, from -100 to 100, and overlapping windows add up. Give it once per window. |
| `Forbid` | Windows the minutes never go in. |

A budget's windows take clock times only, because the day is planned in slots.

A budget holds one device's `Bool` capability at `true`, and that capability needs a `Control`: its `MinimumOn`, `MinimumOff` and `MaximumStartsPerDay` shape the planned runs, and `MinimumOn` and `MinimumOff` must be whole numbers of slots. `oikade check` says if there's no `Control`. Something that must run whatever the budget says, such as frost protection, is a separate `Safety` hold on the same capability, which wins over the budget.

At `PlanningTime` your process plans the day and freezes the plan. The hold is active in the planned blocks, as a Normal claim, so a Safety hold or a Home app tap still beats it. Run minutes count only while the device reports on and is available. The plan and run minutes are saved in the host, so a deploy in the middle of the day doesn't run it again. Editing a budget takes effect at the next `PlanningTime`. Automations that start with no plan for today plan from then, so the hours already gone are lost to the day.

### Arbitration

A capability is arbitrated when your home has a `Control` for it. The host saves that, so a device is arbitrated, and a Safety claim holds, while your automations are down.

```go
framework.NewBundle(
	framework.NewControl(
		HallLight.On.To(false),     // what's left when nobody claims it
		framework.PreferOn,         // two claims at one level: on wins
		framework.ExternalOverride, // a wall switch or Home app tap becomes a claim...
	).OverrideUntilNext(), // ...until what the rules want next changes
	framework.NewHold("hall-motion-light", HallLight.On.To(true)).
		When(func(s framework.State) bool { return s.Get(HallMotion.Occupied) }).
		OffDelay(5*time.Minute),
)
```

Rules, Matter controllers and the device itself make **claims**. The highest level with a claim wins:

| Level | Who claims | Ends |
|---|---|---|
| Safety | Rules with `Priority(framework.Safety)` | A rule's claim ends when its hold ends (for a latched hold, at a `Release`), or as its `Set` says |
| Override | Outside changes (a Matter controller, `oikade set`, or the device by itself), and rules with `Priority(framework.Override)` | An outside change ends after `OverrideHold`. A rule's claim ends as above. |
| Normal | Ordinary rules | As above |
| Default | Nobody | Never |

`framework.NewControl` takes the first three, in order. The rest are chained:

| Input or method | |
|---|---|
| Default | Required. The capability, at its value when nothing claims it: a handle's `To`. |
| Resolve | Required. Which claim wins within a level: `PreferOn` or `PreferOff` for bools, `Highest` or `Lowest` for numbers. Rules have no numeric priorities. |
| External | Required. What an outside change does. `ExternalOverride` makes it a claim for `OverrideHold`. `ExternalRevert` writes the winner straight back, as for a valve that must stay closed. |
| `OverrideHold`, `OverrideUntilNext` | With `ExternalOverride`: a duration, or until the winner without it next changes. |
| `MinimumOn`, `MaximumOn`, `MinimumOff`, `MaximumStartsPerDay` | Equipment limits. They apply after the winner is picked and beat every rule, Safety included. An outside change is accepted and counted, but not fought. |

In this example, motion turns the light on, and it goes off 5 minutes after the motion stops. A wall switch turned off during that is an Override, so the light stays off until the motion hold ends. A wall switch turned on with nobody about lasts until the next motion starts, and the light then goes off 5 minutes after the motion stops. The hold's off-delay is saved, so a restart doesn't leave the light on.

An outside change is recorded by the path it came in by: `Matter` for a command through the bridge, such as a Home app tap, `Command` for `oikade set`, and `Device` for anything else, such as a wall switch. A rule's claim is `Rule/<name>`. Oikade can't tell what a person did, so nothing is called "manual".

### One-off claims

A `Set` on an arbitrated capability is a claim, so it must say when it ends:

```go
framework.NewMoment("towel-rail-boost", TowelRail.On.To(true).For(time.Hour).UntilNext()).
	At(framework.Clock("21:30"))
```

- `For` ends it after that long.
- `UntilNext` ends it when the winner without it next changes. If the rail also had a hold from 06:00 to 08:00, that's when the hold starts the next morning.
- `For` together with `UntilNext` ends it at whichever comes first. This boost ends after an hour, unless what the other claims want changes sooner.
- `UntilReleased` ends it at a `framework.Release("<name>")` action in another rule.
- `Priority(framework.Override)` on the `Set` raises the claim's level. It's Normal otherwise, or in a flow the flow's `Priority`.

A `Set` with none of these on an arbitrated capability of a single device is an error when it runs. A `Set` on a capability that isn't arbitrated is a plain write, and these don't apply to it.

### Latched Safety holds

A latched hold stays active once it has started, until another rule releases it. It's worked out from state rather than from a change. So if the garage overheated while your process was down and is still too hot when it next sees the temperature, it latches then.

```go
framework.NewBundle(
	framework.NewControl(GarageHeater.On.To(true), framework.PreferOff, framework.ExternalRevert),
	framework.NewHold("garage-overheat", GarageHeater.On.To(false)).
		Priority(framework.Safety).
		Latch().
		When(func(s framework.State) bool { return s.Get(GarageTemperature.Temperature) > 40 }),
	framework.NewMoment("garage-overheat-reset", framework.Release("garage-overheat")).
		On(GarageDoor.Open).
		If(func(s framework.State) bool { return s.Get(GarageDoor.Open) }),
)
```

Overheating turns the heater off. Turning it on from the Home app is reverted at once, and the heater stays off after the garage cools, until someone opens the door to check. The Home app keeps showing your tap for about 10 seconds before it shows the heater off again. Oikade has already sent the real value by then, and sending it later doesn't change the wait ([OIK-239](https://linear.app/oikade/issue/OIK-239)). Rules run only while your process is up. If the garage overheats and cools again while it's down, nothing latches. A hold's latch lives under its name, so don't rename it: a deploy refuses that without `--allow-safety-removal`.

### Composing a home

A home is made of parts. Each `Hold`, `Moment`, `Flow`, `Control` and `Plugin` is a `framework.Part`, and `framework.NewHome` takes any number of them after its `framework.Options`. An ordinary Go function can return one, so what belongs together stays together and can move between files. `framework.NewBundle` groups parts, and is a part itself, so bundles nest:

```go
func home() framework.Home {
	return framework.NewHome(framework.Options{Location: framework.NewLocation(51.48, -0.01)},
		lights(),
		dehumidifier(),
	)
}

func lights() framework.Bundle {
	var parts []framework.Part
	for _, z := range zones {
		parts = append(parts, motionLight(z))
	}
	return framework.NewBundle(parts...)
}

func motionLight(z zone) framework.Hold {
	return framework.NewHold(z.name+"-motion-light", z.light.To(true)).
		When(func(s framework.State) bool { return s.Get(z.motion) }).
		OffDelay(5 * time.Minute)
}

// A budgeted hold and the Control it needs.
func dehumidifier() framework.Bundle {
	return framework.NewBundle(
		framework.NewControl(Dehumidifier.On.To(false), framework.PreferOn, framework.ExternalOverride).
			OverrideHold(time.Hour),
		framework.NewHold("dehumidifier", Dehumidifier.On.To(true)).Budget(cellarBudget),
	)
}
```

A helper can be tested on its own: `hometest.New(t, framework.NewHome(framework.Options{}, dehumidifier()), hometest.Options{})`.

- **A rule keeps the name you wrote,** whichever `Bundle` it's in, because a name is the rule's state key. A helper used twice builds a different name each time, and a clash fails `oikade check`.
- **`Options` holds what a home has exactly one of,** so a second location, or one inside a helper, doesn't compile. A home with no sunrise or sunset writes `framework.Options{}`.
- **A nil part, or a pointer to one,** fails `oikade check`, which says where it is.

### Reading status

`oikade status` shows each capability, and for an arbitrated one, the claim that won:

```console
$ oikade status hall-light
hall-light  on  false  Device wants false at Override
```

`oikade status --json` lists every claim, with `expires` when it ends at a set time, `latched` for a latched hold, and `reverted` for an outside change that `ExternalRevert` wrote back. `oikade_device_control_level` has the winning level over time.
