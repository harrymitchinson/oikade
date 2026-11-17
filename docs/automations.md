# Automations

How a home's rules are written: decisions, flows' triggers, conditions, rooms, budgets and arbitration. The [README](../README.md#getting-started-in-go) gets you to a first deploy, and [writing-flows.md](writing-flows.md) covers what a flow does.

## Automations

A home has two kinds of rule ([OIK-298](https://linear.app/oikade/issue/OIK-298)):

- **[Decisions](#decisions)** say what a capability should be, as a list of cases read top to bottom. The engine works out from the clock and the devices' state which case applies, so it stays right across a restart.
- **[Flows](writing-flows.md)** do something when a trigger fires: a capability changes, or a time of day comes. A flow is ordinary Go, with real `if` and `for`, and it can wait for hours and survive a crash.

A capability with a decision is **arbitrated**: its cases, flows, Matter controllers and the device itself make claims, the device takes the winner, and `oikade status` says why. Any other capability is last-write-wins.

### Flows' triggers

```go
home.Flow("hallway-motion-light", func(r *oikade.FlowRun) {
	r.Set(HallwayLight.On.To(true))
	r.Sleep(5 * time.Minute)
	r.Set(HallwayLight.On.To(false))
}).
	Restart(). // new motion restarts the 5 minutes; without it, triggers are ignored while it runs
	When(HallwaySensor.Occupied.Becomes(true)).
	If(oikade.Daily("18:00", "07:00"))
```

`When` takes the changes that trigger it: a `Bool`, a `Number` or a `Choice` fires whenever it changes, a `Bool`'s `Becomes(true)` only when it changes to true, and a [group's or a room's](#groups-and-rooms) on a change from any of its members. `At` takes [times of day](#times-of-day). `If` is a [condition](#conditions), looked at when a trigger fires. For a light that should stay on while there's motion, a [decision](#decisions)'s `Linger` says it more simply.

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

`PoolWater.Temperature.AverageOver(24 * time.Hour)` is a `Number` too: the reading's average over the last day, each value weighted by how long it held. A condition, a [`Follow`](#values-that-follow-a-reading) or a budget's `Bands` reads it like the reading itself. Your process keeps it from each report, and the host saves it, so a restart carries on with it rather than starting again. Until a whole window has gone by, it averages what it has, and while your process is down the last value counts as having held. Make it where the home is built, not inside a `ConditionFunc`, because only those made before the home runs are kept. A budget's `Bands` take it to be as fresh as the device's last report. It never changes as a reading does, so it isn't a flow's trigger. An average of an average, one over less than a minute, or one in `When`, fails `oikade check`.

A condition reading a value nobody has reported yet stops there: a case keeps its state, and a flow's `If` doesn't let it run. If a `Delay` or `Linger` runs out meanwhile, the case starts or ends once the value is back, if its conditions still say so. The log says `Could not evaluate hold; keeping its state until it can` at Info when that starts, with the rule and why, and `Evaluated hold again` when it ends, once each however long it lasts.

A `ConditionFunc` reads the home through its `Snapshot`: `Get` reads a `Bool`, a `Number` or a `Choice`, so `s.Get(BedroomAC.Mode) == oikade.ModeDry` is whether the air conditioner is drying, `Is` says whether another condition is true, `Any`, `All`, `Count` and `Readings` read a [group or a room](#reading-a-room-or-a-group), and `Now` is local time, in the host machine's zone. It must not block, because it holds up every other rule. Its label is how the host names it, since it can't see inside a function to say why it's false. `Lookup` carries on without a value, and reports whether there is one:

```go
oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool {
	v, ok := s.Lookup(CellarSensor.Humidity)
	return ok && v > 65
})
```

A date or time that isn't one, such as `Yearly("1 Jun", "31 Jun")`, `Within(25, 18)` or a `ConditionFunc` with no label, fails `oikade check`, naming the rule.

### Times of day

A flow's `At` fires at times of day, instead of or as well as `When`. `oikade.Clock("23:30")` is a local time, in the host machine's zone, from `"00:00"` to `"23:59"`:

```go
home.Flow("downstairs-bedtime", func(r *oikade.FlowRun) { r.Set(PorchLight.On.To(false)) }).
	At(oikade.Clock("23:30")).
	Late(10 * time.Minute)
```

`oikade.Sunrise` and `oikade.Sunset` are the day's sunrise and sunset, moved by an offset of up to 6 hours either way. They need the home's `Location`, made by `oikade.NewLocation` from its latitude and longitude in degrees (north and east are positive), which your process, `oikade dev`, `oikade replay` and `hometest` all read from the `Home`:

```go
home := oikade.NewHome(oikade.Options{Location: oikade.NewLocation(51.48, -0.01)}) // round to 2 decimals
home.Flow("porch-at-dusk", func(r *oikade.FlowRun) { r.Set(PorchLight.On.To(true)) }).
	At(oikade.Sunset(-20 * time.Minute)) // 20 minutes before sunset
```

Two decimals are about a kilometre, which moves sunset by seconds, so your home's repo needn't hold more than its postcode. A home with no sun times needs no `Location`. `NewLocation` takes both coordinates, so one can't be forgotten, and 0 is a coordinate like any other.

- **A time that isn't one,** such as `Clock("25:00")`, a sun time with no location, or an offset over 6 hours, fails `oikade check`, naming the rule.
- **A `Daily` takes two strings or two times,** so one with a sun edge writes its clock edge as `oikade.Clock("23:30")`.
- **Several times** fire at each, so `At(Clock("18:00"), Sunset(0))` fires at both, and the same time twice fires once.
- **A day the sun doesn't rise or set,** as near the poles, that time doesn't fire, and a window with it as an edge stays shut.
- **A `Daily` with a sun edge and a clock edge** ends at the first end after its start, as any `Daily` does. So in midsummer `Daily(Sunset(3*time.Hour), Clock("23:30"))` starts after midnight, and stays open until 23:30 that evening. Keep the offset small enough that the order holds all year.
- **A time the clocks skip** when daylight saving starts fires when they go forward, and a time that happens twice when it ends fires the first time.
- **`Late`** is how late a time may still fire after your process was down at it, 10 minutes by default. A later one is skipped, logged and counted as `missed`. A flow a decision ranks above its override line fires however late it is. Only the latest missed time counts.

### Groups and rooms

A group is some devices together, under a name of its own, and a room is the devices in a room now. Each has a field for every capability, a `Bool`, a `Number` or a `Choice` as a device does, which a decision, a flow's `Set` and a trigger take, and a `Bool`'s `Any()` and `All()` are [conditions](#conditions) ([OIK-298](https://linear.app/oikade/issue/OIK-298)):

```go
radiators := home.Group("radiators", BedroomRadiator, StudyRadiator, KitchenRadiator)
windows := home.Group("windows", BedroomWindow, StudyWindow, KitchenWindow)

home.Decide(radiators.Setpoint).
	Case("windows-open", 7, windows.Open.Any()).Delay(2 * time.Minute).
	OverrideFor(2 * time.Hour).
	Follow("weather", OutdoorSensor.Temperature, compensate).
	Default(18)

home.Flow("lights-out", func(r *oikade.FlowRun) {
	r.Set(home.Room("living-room", oikade.Lights).On.To(false)) // the lights, not the plugs
}).When(home.Room("hallway").Occupied) // any sensor in the hallway
```

A group's members are devices from `devices.go`, which `oikade types` writes, and a group's capability covers the members that have it. So `radiators.Humidity` compiles, and `oikade check` refuses it, since no radiator has one. A group's name is how `oikade status` and `oikade pause` name its decision's cases, `radiators setpoint: windows-open`, so it can't be a device's name too.

A room's devices change as a device is paired, removed or [declared](devices.md#declaring-devices-in-code) in another room, so what a room covers is worked out each time it's read or written. A lamp moved into the room later is covered, and so is one paired there. Its kinds say which of the room's devices you mean, by their [device types](devices.md#what-a-device-is), because `on` is a light's, a plug's and a heater's alike, and "lights on at dusk" shouldn't turn on the heater ([OIK-255](https://linear.app/oikade/issue/OIK-255)):

| Kind | Device types |
|---|---|
| `oikade.Lights` | `OnOffLight`, `DimmableLight` |
| `oikade.Plugs` | `OnOffPlugInUnit`, `DimmablePlugInUnit`, `MountedOnOffControl`, `MountedDimmableLoadControl` |
| `oikade.Heaters` | `Thermostat`: any device with a setpoint |
| `oikade.Kinds(...)` | the types you list, such as `oikade.Kinds(oikade.DimmableLight)` |
| `oikade.Everything`, or none | every device in the room with the capability, whatever it is |

A device whose type isn't one of the room's kinds is left alone, and the log names it, so a wall switch typed `MountedOnOffControl` is a plug until your home declares it a light with `Is`. A room's sensors have no kind of their own: read and trigger on them with none.

#### A decision on a group or a room

A group's decision is each member's: each member with the capability is arbitrated by it, and its cases write them all, with one delay and one latch between them. A tap on one radiator is an override on that radiator only. Each member has its own [limits](#arbitration), the group's `Limits`. A budget is per device, so a group's decision can't have one.

A member can have lines of its own above the group's, and end its decision with `Then` in place of `Default`:

```go
home.Decide(StudyRadiator.Setpoint).
	Case("working", 21, StudySensor.Occupied, oikade.Daily("09:00", "17:30")).
	Then(radiators.Setpoint)
```

The study's radiator is at 21 while someone works there, and otherwise as the group's decision says. Its own lines rank above all of the group's, so with no override line of their own, they're safety lines, and an open window doesn't turn the radiator down while someone works there. Give it an override line to rank them below a tap, and it takes the group's place for this radiator. The group's `Default` and `Limits` are the member's. Without `Then`, `oikade check` refuses a decision on a member of a decided group, and a device in two decided groups.

A room's decision is the host's for each device in the room of its kinds that has the capability, and the host keeps it with the room. So a device moved in is arbitrated by it, and one moved out isn't. A device in the room with a decision of its own keeps it, unless that decision hands over to the room's with `Then`.

#### Reading a room or a group

A `ConditionFunc` reads a room or a group with its `Snapshot`'s `Any`, `All` and `Count` for a `bool` capability, and `Readings` for a number, whose values go to `slices.Max`, `slices.Min` or an average:

```go
// A dehumidifier that runs on whichever cellar sensors have reported.
cellar := home.Room("cellar")
oikade.ConditionFunc("cellar damp", func(s oikade.Snapshot) bool {
	return slices.Max(s.Readings(cellar.Humidity)) > 65
})
```

A read covers the members that have reported the capability, and ignores the rest, so one dead sensor doesn't stop a rule over three. A group's capability isn't a condition on its own: `oikade check` asks for `Any` or `All`. If none has reported, the condition stops, as at a `Get`. A case that reads a room looks again when a device moves in or out. A sensor that reported and then died keeps its last value.

A room with no devices in it, or none of the read's kinds, stops the condition too, and a case keeps its state: one that applied, or was lingering, stays until a device is back in the room. The log says `Rule read a Room with no members` at Warn, once each time it happens, with the rule and the room. `oikade devices remove` still removes a device when that leaves a room a case reads with nothing of the read's group in it, but it warns on stderr, naming the room and the cases, and `--json` has the warning as `warning`.

#### Triggers and writes

- **A group's or a room's trigger** fires on a change from any member: for a room, any device in the room and of its kinds at the time of the change.
- **A group's `Set`** writes each member with the capability, each ranked by its own decision, and one that fails doesn't stop the others.
- **A room's `Set`** sets every device in the room and of its kinds that has reported the capability, in name order, and skips the rest. So `on` on a room with no kinds leaves the motion sensor alone, and turns off a heater along with the lamps, while `Lights` leave the heaters alone ([OIK-20](https://linear.app/oikade/issue/OIK-20)). Membership is read when the `Set` runs, so a lamp added to the room during a `Sleep` is included in the `Set`s after it.
- **If some devices fail**, the others are still set and the flow carries on, so a dead bulb can't leave the rest of the room on. The run counts as `failed`. A room with nothing to set counts as `succeeded`.
- **`Restart` with a room trigger** restarts the run on a change from any sensor in the room, which suits people moving between sensors. Drivers only report real changes, so someone standing still in front of one sensor doesn't restart the run. Keep the `Sleep` longer than the time your sensor holds occupancy after the last movement.
- **A room `Set` with no end skips arbitrated members,** because a claim must end (see [claims from flows](#claims-from-flows)). The log names them, with members of other types, as `Skipped Room members`.

### Decisions

A decision is everything that sets one capability, in one list read top to bottom. `home.Decide` takes the capability, a device's or a [group's](#groups-and-rooms), and each line after it says when it applies and what it wants. The first line that applies wins, and `Default`, the last line, is what's left when none does ([OIK-298](https://linear.app/oikade/issue/OIK-298)):

```go
home.Decide(BathroomFan.On).
	Case("humid", true, BathroomSensor.Humidity.Above(70)).Linger(10 * time.Minute).
	Case("night", false, oikade.Daily("23:00", "07:00")).
	Default(false)
```

A case takes a name, its value and its [conditions](#conditions), all of which must be true. Its name is scoped to its capability, so two devices can each have a `night`, and Oikade shows it as `bathroom-fan on: night`. What follows a case changes it:

| Method | |
|---|---|
| `Delay` | How long its conditions must stay true before it applies. |
| `Linger` | How long it still applies after they stop being true. |
| `LatchUntil(c)` | Once it applies, it keeps applying until `c` is true. See [below](#latches). |

The rest are lines of their own:

| Line | |
|---|---|
| `Budget(name, true, b, conditions…)` | Runs a `Bool` for a number of minutes a day, in the slots that suit it best, while its conditions are true. See [Budgets](#budgets). |
| `Follow(name, reading, f)` | Sets a number to `f` of a reading, such as a setpoint from the temperature outside. `.Step(n)` after it sets what it's rounded to. See [below](#values-that-follow-a-reading). |
| `OverrideFor(d)`, `OverrideUntilNext()`, `NoOverride()` | Where a tap in the Home app, a wall switch or `oikade set` ranks. See [Arbitration](#arbitration). |
| `Flows()`, `Flows(f)` | Where every flow's `Set` ranks, or one flow's. See [Claims from flows](#claims-from-flows). |
| `Limits(oikade.Limits{…})` | Equipment limits, which beat every line. |
| `Default(v)` | Required, and last: the value when no line applies. |
| `Then(group.Capability)` | In place of `Default`, on a member of a group or a room: the group's decision, when none of the member's lines applies. See [Groups and rooms](#a-decision-on-a-group-or-a-room). |

A capability has one decision, and `oikade check` refuses a second, a line after `Default`, a case with no condition and a name given twice. A case's conditions are looked at again on every device change and at least once a minute. A `Daily` in them, on its own or in an `All`, is different: the case starts and ends on its edge, and `oikade status` shows when it next does. One inside an `Any` or a `Not` changes within a minute of its edge.

- **Every capability with a decision is arbitrated:** the host saves the decision, so a device is arbitrated, and a safety line holds, while your automations are down. A setting you delete goes once that deploy has settled, and a deploy that would end a safety line's claim that way needs `--allow-safety-removal`.
- **Restarts are safe.** The host saves each case's state, under its device, capability and name, and a new process writes only if a case started or ended meanwhile. A `Linger`'s deadline is saved too, so the fan still stops on time. Renaming a case loses its state, so a deploy refuses to drop a safety line's that way without `--allow-safety-removal`.
- **A lock** goes to its `Default` when nothing claims it, as any capability does, so give it `Default(true)` unless it should unlock by itself ([OIK-23](https://linear.app/oikade/issue/OIK-23)).

### Arbitration

Each line of a decision claims the capability while it applies, and the device takes the winner. The override line says where an outside change ranks: a Matter controller such as the Home app, `oikade set`, or the device by itself, such as a wall switch.

```go
home.Decide(HallLight.On).
	Case("motion", true, HallMotion.Occupied).Linger(5 * time.Minute).
	OverrideUntilNext(). // a tap wins until what the cases want next changes
	Default(false)
```

| Override line | An outside change |
|---|---|
| `OverrideFor(d)` | beats the lines below it for `d` |
| `OverrideUntilNext()` | beats the lines below it until their answer next changes |
| `NoOverride()` | is written back at once, as for a valve that must stay closed. It goes before the first line |
| none | beats every line until their answer next changes |

A line above the override line is a **safety line**: an outside change can't beat it, and with `NoOverride`, every case is one. Pausing a safety line needs `--allow-safety`, and a deploy that would end one's claim needs `--allow-safety-removal`. With no override line, nothing is a safety line, and `oikade check` warns about a latched case, which a tap could then beat until it lets go.

In this example, motion turns the light on, and it goes off 5 minutes after the motion stops. A wall switch turned off during that is an override, so the light stays off until the motion case ends. A wall switch turned on with nobody about lasts until the next motion starts, and the light then goes off 5 minutes after the motion stops.

`Limits` apply after the winner is picked and beat every line, safety lines included: `MinimumOn` and `MinimumOff` are how long a run and a rest last at least, `MaximumOn` stops a longer run, and `MaximumStartsPerDay` caps the starts. An outside change is accepted and counted, but not fought. `oikade status` says when a limit holds a line back: `frost wants true, resting until 03:20`.

A report soon after a write is the device catching up, not an outside change: for 5 seconds, or as long as the capability takes to settle, which is 2 minutes for a blind's `position` and 30 seconds for `locked` ([OIK-23](https://linear.app/oikade/issue/OIK-23)). So a blind reporting 20, 40 and 60 on its way to 70 isn't an override. A report of something else once that's up is, so a blind that someone stopped at 45 is an override at 45, two minutes after the write. A device's first report after the host starts isn't one either: a change made while the host was down is undone, and the winner written again.

An outside change is recorded by the path it came in by: `Matter` for a command through the bridge, such as a Home app tap, `Command` for `oikade set`, and `Device` for anything else, such as a wall switch. A case's claim is `Rule/<device> <capability> <case>`. Oikade can't tell what a person did, so nothing is called "manual".

### Claims from flows

A flow's `Set` on a decided capability is a claim at the decision's `Flows()` line:

```go
home.Decide(TowelRail.On).
	Case("morning", true, oikade.Daily("06:00", "08:00")).
	OverrideFor(time.Hour).
	Flows().
	Default(false)

home.Flow("towel-rail-boost", func(r *oikade.FlowRun) {
	r.Set(TowelRail.On.To(true).For(time.Hour).UntilNext())
}).At(oikade.Clock("21:30"))
```

`Flows(f)` is a line for one flow, the one `home.Flow` returned, so it can rank above a tap while the rest rank below it. A flow's own line counts before the line for every flow. Above the override line, it's a safety line, its claims are Safety, and the flow acts however late it is:

```go
leak := home.Flow("leak", func(r *oikade.FlowRun) {
	r.Set(MainsValve.On.To(false).UntilReleased())
}).When(SinkSensor.Humidity).If(SinkSensor.Humidity.Above(90))

home.Decide(MainsValve.On).
	Flows(leak).            // this flow beats a switch
	OverrideFor(time.Hour).
	Flows().                // every other flow
	Default(true)
```

A `Set` on a decision with no `Flows` line for its flow fails the run, and that's logged. `oikade check` can't see that, because a flow's `Set`s are only known as it runs.

A claim must say when it ends:

- `For` ends it after that long.
- `UntilNext` ends it when the winner without it next changes. Here, that's when the morning case starts.
- `For` together with `UntilNext` ends it at whichever comes first. This boost ends after an hour, unless what the other lines want changes sooner.
- `UntilReleased` ends it at an `r.Release("<name>")` in a flow, its own or another's.

Two claims on one line, the later wins. A `Set` with none of these on a decided capability of a single device is an error when it runs. A `Set` on a capability with no decision is a plain write, and these don't apply to it.

### Latches

A latched case keeps applying once it has started, until its `LatchUntil` condition is true. It's worked out from state rather than from a change, so if the garage overheated while your process was down and is still too hot when it next sees the temperature, it latches then.

```go
home.Decide(GarageHeater.On).
	NoOverride().
	Case("overheat", false, GarageTemperature.Temperature.Above(40)).LatchUntil(GarageDoor.Open).
	Default(true)
```

Overheating turns the heater off. Turning it on from the Home app is reverted at once, and the heater stays off after the garage cools, until someone opens the door to check. No other rule is needed to reset it. The Home app keeps showing your tap for about 10 seconds before it shows the heater off again. Oikade has already sent the real value by then, and sending it later doesn't change the wait ([OIK-239](https://linear.app/oikade/issue/OIK-239)). Rules run only while your process is up. If the garage overheats and cools again while it's down, nothing latches. An `r.Release("garage-heater on overheat")` in a flow ends the latch too.

### Values that follow a reading

A `Follow` line sets a number capability to a function of a reading, such as weather compensation for a radiator:

```go
home.Decide(StudyRadiator.Setpoint).
	Case("windows-open", 7, StudyWindow.Open).Delay(2 * time.Minute).
	OverrideFor(2 * time.Hour).
	Flows().
	Follow("weather", OutdoorSensor.Temperature, func(outside float64) float64 { return 21 - outside/4 }).
	Default(18)
```

The value is rounded to the capability's step, 0.5 for a setpoint and 1 for brightness and position, or to `.Step(n)`'s, and kept within the capability's bounds. It's written only when the value is three quarters of a step from the last one written, so a reading that wobbles across a rounding edge isn't a write each time. The host saves the last one written, so a restart doesn't write it again. A function that gives NaN or an infinity counts as an unreported reading. The reading can be an [`AverageOver`](#conditions). Until it's reported, the lines below apply, and `oikade status` says why. A `Follow` is a line like a case, named `study-radiator setpoint: weather`, so `oikade pause study-radiator setpoint weather` pauses it.

### Budgets

A budget line runs a device for a number of minutes a day, and plans when. Here a loft fan runs one to five hours a day depending on how humid the loft was at midnight, overnight where it can, and never in the evening:

```go
home.Decide(LoftFan.On).
	Limits(oikade.Limits{
		MinimumOn:           time.Hour,        // each run at least an hour
		MinimumOff:          30 * time.Minute, // and half an hour's rest between runs
		MaximumStartsPerDay: 3,
	}).
	OverrideFor(time.Hour).
	Budget("airing", true, oikade.NewBandedBudget(oikade.Bands{
		Reading: LoftHumidity.Humidity,
		MaxAge:  time.Hour, Missing: 180, // if the sensor hasn't reported in the last hour
		Ranges:    []oikade.Band{{Below: 55, Minutes: 60}, {Below: 70, Minutes: 180}},
		Otherwise: 300,
	}).
		Prefer(oikade.Daily("23:00", "07:00"), 3).
		Forbid(oikade.Daily("18:00", "22:00"))).
	Default(false)
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

A budget's conditions come after it, as a case's do: `Budget("summer-extra", true, extra, oikade.Yearly("1 Jun", "15 Sep"))` runs only in summer. The day is planned whatever they say, and a planned slot they rule out isn't made up later. A condition that can't be told yet, such as one reading a device that hasn't reported, counts as false, so the budget stops rather than running on. A `Bands` reading can be an [`AverageOver`](#conditions), such as the water's temperature over the last day.

A budget's `Daily`s take clock times only, because the day is planned in slots. A budget sets a `Bool` to `true`. The decision's `Limits` shape the planned runs, and `MinimumOn` and `MinimumOff` must be whole numbers of slots. Something that must run whatever the budget says, such as frost protection, is a case above it.

At `PlanningTime` your process plans the day and freezes the plan. The line applies in the planned blocks, so a line above it or a Home app tap still beats it. Run minutes count only while the device reports on and is available. The plan and run minutes are saved in the host, so a deploy in the middle of the day doesn't run it again. Editing a budget takes effect at the next `PlanningTime`. Automations that start with no plan for today plan from then, so the hours already gone are lost to the day.

### Composing a home

`oikade.NewHome` makes a home from its `oikade.Options`, and everything else is added to it: `home.Decide` adds a decision, `home.Flow` a flow and `home.Plugin` a [plugin](plugin-contract.md). `home.Run()` runs it. An ordinary Go function can take the home and add to it, so what belongs together stays together and can move between files:

```go
func home() *oikade.Home {
	home := oikade.NewHome(oikade.Options{Location: oikade.NewLocation(51.48, -0.01)})
	bedtime(home)
	for _, z := range zones {
		motionLight(home, z)
	}
	return home
}

func motionLight(home *oikade.Home, z zone) {
	home.Decide(z.light).
		Case("motion", true, z.motion).Linger(5 * time.Minute).
		Default(false)
}

func main() {
	home().Run()
}
```

A helper can be tested on its own, in a home of its own.

- **A rule keeps the name you wrote,** because a name is the rule's state key. A helper used twice builds a different name each time, and a clash fails `oikade check`.
- **A helper that adds a flow can return it,** for a decision elsewhere to rank with `Flows(f)`.
- **`Options` holds what a home has exactly one of,** so a second location, or one inside a helper, doesn't compile. A home with no sunrise or sunset writes `oikade.Options{}`.

### Reading status

`oikade status` shows each capability, and the line that won it:

```console
$ oikade status pool-pump
pool-pump  on  true  frost wants true
```

Its rules say what each case is doing, and why one doesn't apply:

```console
pool-pump on: frost  case  Safety  inactive: pool-water temperature is 12, not below 3
```

A `ConditionFunc` shows only its label. `oikade status --json` names the `case` that won, and lists every claim, with `expires` when it ends at a set time, `latched` for a latched case, and `reverted` for an outside change that `NoOverride` wrote back. `oikade_device_control_level` has the winning level over time: `Safety` for a safety line, `Override` for an outside change, `Normal` for the rest, and `Default`.
