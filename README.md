# Oikade

Home automation as code you can test, on one small machine.

Your home is a Go module: holds, moments and flows that compile, run in tests on a fake clock, and deploy without stopping anything. `oikade-host` is one binary that keeps your devices, runs a Matter bridge for the Home app and other controllers, and supervises your code. Prometheus keeps the history.

| | Owner | Examples |
|---|---|---|
| **Your automations** | Git, as a Go module | What should happen: holds, moments, flows, arbitration |
| **Devices and state** | The host's data directory | Which devices exist, their names and rooms, claims, latches, Matter's pairing |
| **History** | Prometheus and Grafana | Temperature over time, how often the door opened, rule runs |

Toggling a light from your phone is state, so Git doesn't care, and a deploy doesn't touch it.

## Components

```
 drivers                      oikade-host                          consumers
 ┌──────────────┐    ┌───────────────────────────────┐    ┌──────────────────────────────┐
 │ zigbee2mqtt  │◀──▶│ device state, arbitration,    │◀──▶│ your automations (a process  │
 │ plugin       │    │ the store, the Matter bridge, │    │ the host runs and replaces)  │
 ├──────────────┤    │ metrics, the local API        │───▶│ the Home app, Google Home, … │
 │ your plugins │◀──▶│                               │───▶│ Prometheus                   │
 └──────────────┘    └───────────────────────────────┘    └──────────────────────────────┘
```

- **`oikade-host`** is long-lived. It owns everything that must survive, and serves Matter, metrics on `:8080` and a local API. Run one per home.
- **Your process** is your module's binary. The host starts it, restarts it if it dies, and swaps it on a deploy, while Matter stays up and your devices stay as they are.
- **Plugins** are processes too, each supervised by the host, so one that hangs is restarted while the rest carry on.
- **`oikade`** is the command you run, on your laptop or in CI: `init`, `test`, `dev`, `deploy`, `status` and the rest. It reaches the host over its API, and carries nothing of the host itself.

## Requirements

- **Linux with systemd, or Docker,** on arm64 or amd64. A Raspberry Pi is plenty: see [One Pi, with the host](docs/deploy-raspberry-pi.md).
- **The same L2 network as your controllers, with IPv6.** Controllers find the bridge over mDNS and reach it over IPv6.
- **A Matter controller with a hub,** such as the Home app with an Apple TV or HomePod as its home hub.
- **For Zigbee,** a coordinator, and a Zigbee2MQTT and an MQTT broker you run. See the [Zigbee2MQTT plugin](plugins/zigbee2mqtt/README.md).
- **Go,** wherever you build your home.

## Getting started in Go

A home is a Go module. You need Go and `oikade`, and no device or host:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikade@main
mkdir home && cd home
oikade init
oikade test
```

Or take `oikade` from a [release](https://github.com/harrymitchinson/oikade/releases): `oikade-<os>-<arch>.tar.gz`, for darwin or linux on arm64 or amd64, holds it with this README and the licence, and `checksums.txt` has each archive's SHA-256.

`init` writes a module with one automation, its test, a `devices.go` with two example devices, and a GitHub Actions workflow. The workflow runs `oikade check` and `oikade test` on every change, and `oikade deploy` once one reaches `main`, over the [network API](#the-network-api). It needs the secret `OIKADE_TOKEN`, from `oikade-host client add ci --token`, and the variables `OIKADE_ADDRESS`, such as `pi.home:8443`, and `OIKADE_PIN`, which that command prints. GitHub's own runners can reach the host only if its API is on the internet, which it shouldn't be, so run the job on a self-hosted runner on your network, or join the runner to your network over a VPN first. `oikade types` replaces `devices.go` once you have real devices.

```go
func home() framework.Home {
	return framework.NewHome(framework.Options{},
		framework.NewHold("porch-light", PorchLight.On.To(true)).
			When(func(s framework.State) bool {
				return s.Get(PorchSensor.Occupied)
			}).
			OffDelay(5*time.Minute),
	)
}
```

`devices.go` has a value for each device: `PorchLight.Name` is its name, and `PorchLight.On` is a handle on one of its capabilities, typed `bool` or `float64`. Your editor lists what each device can do, and `s.Get` reads a handle, `To` writes one, and a `Change` is one changing. The capabilities are constants in `framework`, such as `framework.On`, for [rooms](#rooms).

`oikade check` finds what's wrong before anything runs: the compiler, so a misspelt device, a capability the device doesn't have or a value of the wrong type doesn't build, then [flows that wouldn't replay](docs/writing-flows.md) and a rule's chained method whose result isn't used, then the checks a deploy makes, which include an option given twice. `oikade deploy` makes these checks too. If it can reach your host, it also lists the devices a deploy would leave behind because their [plugin](#removing-a-plugin) is no longer imported.

`oikade dev` runs your automations against a simulated home built from `devices.go`, on a clock you move by hand:

```text
$ oikade dev
Simulating 2 devices from Fri 18:41. Type help for the commands.
set porch-sensor occupied true
Fri 18:41 Wrote porch-light on = true
Fri 18:41 Hold porch-light started
set porch-sensor occupied false
advance 6m
Fri 18:46 Wrote porch-light on = false
Fri 18:46 Hold porch-light ended
```

`status` shows every device and what holds it, and `restart` crashes your automations and starts them again. Your automations' log goes to standard error.

`oikade test` is `go test`. The `hometest` package runs the real engine, arbiters and planner in your test: set device values, move a fake clock, and look at writes and claims. A restart in the middle of a flow is one call:

```go
func TestBedtimeSurvivesACrash(t *testing.T) {
	h := hometest.New(t, home(), hometest.Options{})
	h.Advance(22*time.Hour + 40*time.Minute) // Into the flow's 30-minute wait.
	h.Restart()
	h.Advance(21 * time.Minute)
	h.ExpectValue(Lounge.On, false)
	h.ExpectFinished("bedtime")
}
```

The clock starts at midnight UTC on a Monday, unless `Options.Start` says otherwise. A mistake in a condition goes to the test's log, which `go test` shows when it fails.

`h.Set` changes one value. A sensor often reports several at once, and your rules see all of them before any acts, so a Safety hold on one is never late for a Normal hold on another. `h.Report(Hall.Humidity.To(72), Hall.Occupied.To(true))` sends one device's values as one report, as the host does.

`oikade replay` shows what your code would have done last week. It fetches the host's event log, runs the code in your working copy against it on a fake clock, and compares that with what the deployed version did:

```text
$ oikade replay
Replayed Fri 30 Oct 19:02:11 to Fri 6 Nov 19:02:11: 161240 changes. The deployed version wrote 412 times, your code 414.
- Mon 2 Nov 07:12:40 bathroom-fan on = true
+ Mon 2 Nov 07:04:10 bathroom-fan on = true
```

A `-` is a write the deployed version made and your code doesn't, and a `+` the reverse. Two writes within 5 seconds count as the same one. Unchanged code shows `No differences.` `--since 48h` compares a shorter time, though your code always runs from the start of the log so that it gets there in the right state. `--json` prints the report. A week of 40 busy devices took about a minute on a laptop, so a full 14-day log takes about two.

A replay never reaches a device: your code runs in `simhome`, as in `oikade dev`, and asks the host only for its status and its log. A flow's `Do` isn't called. It gets the answer the deployed version got, or none if that call isn't on record, and the report says so. What it can't know comes as a note: a pause, a deploy, or your automations being down. It also runs a rule the deployed version had paused, a device that went quiet counts as alive, and a host that died outright leaves no note. Anything your code prints goes to standard error, with its errors. A device renamed during the log looks like two devices.

The log keeps each change, write and rule run, and is bounded by age and size, oldest first. `oikade-host --event-log-age 336h --event-log-mib 64` are the defaults. Prometheus stays the place for history: this exists to be replayed, and losing it costs nothing else.

Arbitration and budgets are parts of the same home, and a [`Bundle`](#composing-a-home) keeps them together. `Control` is [arbitration](#arbitration), and a `Hold` with a `Budget` is [a budget](#budgets):

```go
framework.NewBundle(
	framework.NewControl(Dehumidifier.On.To(false), framework.PreferOn, framework.ExternalOverride).
		OverrideHold(2*time.Hour).
		MinimumOn(time.Hour).
		MinimumOff(30*time.Minute).
		MaximumStartsPerDay(3),
	framework.NewHold("dehumidifier-daily", Dehumidifier.On.To(true)).
		Budget(framework.NewBandedBudget(framework.Bands{
			Reading: Cellar.Humidity, Missing: 240,
			Ranges: []framework.Band{{Below: 55, Minutes: 60}, {Below: 65, Minutes: 180}}, Otherwise: 300,
		}).
			Prefer(framework.NewWindow(framework.Clock("22:00"), framework.Clock("06:00")), 3).
			Forbid(framework.NewWindow(framework.Clock("07:00"), framework.Clock("08:30")))),
)
```

`framework.NewBudget` takes a function of the home at planning time, instead of `Bands`. If it panics or reads a device that hasn't reported, the day's budget is zero, and the log says why. A budget needs a `Control`, because it plans around the limits there, and `oikade check` says so if there isn't one. The host saves your `Control`, so a device is arbitrated, and a Safety claim holds, while your automations are down. A setting you delete goes once that deploy has settled, and a deploy that would end a Safety claim that way needs `--allow-safety-removal`. A budget's plan and run minutes are saved too, so a deploy in the middle of the day doesn't run it again.

## Automations

A home has two kinds of rule, plus [flows](docs/writing-flows.md) for anything with a decision or a long wait in it ([OIK-64](https://linear.app/oikade/issue/OIK-64)):

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

`framework.InRoom` is a capability of every device in a room, as a `Change`, a condition's read, or with `To` as a `Set` or a hold's `Set`. A device is in a room when the [registry](#devices) says so, so a lamp added to the room later is covered without a deploy, which is why a room takes a capability rather than handles.

```go
framework.NewMoment("lights-out",
	framework.InRoom("living-room", framework.On).To(false), // everything there that has on
).On(framework.InRoom("hallway", framework.Occupied)) // any sensor in the hallway
```

A room without types means every device in it with the capability, and `on` is a light's, a switch's and an outlet's alike, so that turns off the fridge's plug too. `Types` limits a room write, a moment's or a hold's, to devices of some [device types](#what-a-device-is):

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

### What a device is

A device's capabilities say what it can do, and its device types say what it is, in Matter's names ([OIK-142](https://linear.app/oikade/issue/OIK-142)). `oikade devices` shows them in its `DEVICE TYPES` column, and `--json` as `deviceTypes`. Only a device that's `on` or off can be more than one thing, so that's the one you choose: `OnOffLight`, `DimmableLight`, `OnOffPlugInUnit`, `DimmablePlugInUnit`, `MountedOnOffControl` (a switch in the wall) or `MountedDimmableLoadControl` (a dimmer in the wall). A sensor's types follow from its capabilities, so a motion sensor with a thermometer is an `OccupancySensor` and a `TemperatureSensor`.

Yours wins, then its driver's, if the driver knows: a Zigbee2MQTT light is an `OnOffLight`, or a `DimmableLight` with a brightness, and a plugin may report one. A Zigbee2MQTT switch doesn't say, because a relay can't tell what it switches. Otherwise a device with `brightness` is a `DimmableLight`, and one without an `OnOffPlugInUnit`, because a bare relay could be driving anything. `oikade devices type DEVICE ""` goes back to what its driver says. A dimmable type needs `brightness`, and an on/off type on a device that has it leaves dimming out of Matter. Changing a device's type so that a Safety rule's room write limited to types would leave it out, such as a plug a Safety hold keeps on that you now call a light, needs `--allow-safety`, and the error names the rule. That's Safety holds and moments: what a flow writes isn't known until it runs. The type you set stays with the name, as a room does, through `replace`, a restart, `oikade types` and a restore. A reserved name takes any of the six, and once it's paired, a type its hardware can't be falls back as above.

## Capabilities

Drivers translate to and from a protocol-neutral set of capabilities, named as a person would say them ([OIK-181](https://linear.app/oikade/issue/OIK-181)). Matter, metrics and rules only ever see these, and any other name is unknown. A device with several sensors is one Matter endpoint with several device types, and a battery is a Power Source on it. [docs/capabilities.md](docs/capabilities.md) lists each one's range and Matter attribute, generated from the registry.

| Capability | Type | Writable | Matter |
|---|---|---|---|
| `on` | bool | ✓ | Its [device type](#what-a-device-is): a light, a plug-in unit or a wall control |
| `brightness` | 0–100 | ✓ | Level Control, on a dimmable device type |
| `temperature` | °C | | Temperature Sensor |
| `humidity` | % | | Humidity Sensor |
| `open` | bool | | Contact Sensor (Boolean State, `true` for closed). `true` means open, the inverse of Zigbee2MQTT's `contact`. |
| `occupied` | bool | | Occupancy Sensor (passive infrared) |
| `battery` | % | | Power Source, with a warning below 20% |
| `link-quality` | 0–255 | | (metrics only) |

## Plugins

A plugin connects a device platform that isn't Zigbee, such as the eWeLink LAN protocol or the Overkiz cloud API, without changing Oikade. It targets the platform, not one device: a new device on that platform is a model in the plugin, not a new plugin. Devices it serves are ordinary devices: they show up on the Matter bridge, in Grafana and in rules like Zigbee ones.

On a host, a plugin is a Go module your home imports, as a `framework.Plugin` part of your home, and the host runs it as a process of its own, so one that panics or hangs is restarted while automations and Matter carry on. It runs as the host's user, so **importing a plugin means trusting its code**. A platform with no good Go library runs as an outside process speaking the contract to a broker of yours, with `oikade-host --broker URL --outside-plugins NAME`. The [contract](docs/plugin-contract.md#a-plugin-your-home-imports) says how.

First-party plugins, each with its own README:

- [Zigbee2MQTT](plugins/zigbee2mqtt/README.md): Zigbee devices, through a Zigbee2MQTT and an MQTT broker you run
- [eWeLink LAN](plugins/ewelinklan/README.md): relays with sensors on the eWeLink LAN protocol, such as SONOFF's TH Elite

To write a plugin in Go, use the [SDK](sdk/plugin) and the contract types in [`contract/v1`](contract/v1). A plugin's whole `main` is one `plugin.Run` call, and the SDK handles the Last Will, re-announcing after a reconnect, `/set` and SIGTERM. First-party plugins live in [`plugins/`](plugins), and anyone else's in its own repo, `oikade-plugin-<name>`, with its own images and releases. [`test/fixtures/simswitch`](test/fixtures/simswitch) is a complete plugin with simulated relays, which this repo's conformance tests drive. A plugin in another language implements the [contract](docs/plugin-contract.md) itself. Any plugin repo can check itself against the contract with [`oikade-conformance`](docs/plugin-contract.md#conformance).

### Removing a plugin

Taking a `framework.Plugin` out of your home doesn't remove its devices ([OIK-225](https://linear.app/oikade/issue/OIK-225)). They stay on record with their names, rooms, claims and Matter endpoints, offline, and the Home app shows them as not responding, so that a plugin missing from one build, or a rollback, loses nothing. `oikade check` and `oikade deploy` list those devices, and call out any a Safety claim holds. `oikade devices` and `oikade status` mark them `plugin not imported`. Import the plugin again and they come back as they were. To be rid of them, `oikade devices remove --plugin NAME` removes all of that plugin's devices at once, as `remove` does one, and needs `--allow-safety` if a Safety claim holds any of them.

## Deploying

Your home is a Go module, and this replaces the running version from it:

```sh
oikade deploy --data-dir /var/lib/oikade --module .
```

That rejects code that would break a flow's replay, builds your module for the host, and hands it over, with a line on stderr as each part starts. stdout has only the result:

```console
$ oikade deploy --data-dir /var/lib/oikade --module .
Checking your code
Building for linux/arm64
Uploading 11 MB: 11 MB sent
Waiting for the host to check and settle
Deployed version 3f9c2a1
```

A build that takes a while says it's still going every 5 seconds, and on a terminal the upload counts its bytes as it goes. Anywhere else, such as a pipe or CI's log, the lines come whole, with no counter. The host runs its check, then swaps it in with automations stopped for about a second and your device state untouched. Holds are worked out again, and waiting flows carry on from where they were. A flow whose next step changed is dropped and logged. If the new version crashes within the settle period, 5 seconds by default, the last one comes back and the command fails.

```sh
oikade rollback --data-dir /var/lib/oikade
```

returns to the version before the running one. The host keeps the last five.

Removing a Safety rule is refused, and so is renaming one, because a rule's claims end with it and a hold's latch lives under its name, which could reopen a valve. `--allow-safety-removal` on either command goes ahead. `--binary FILE` deploys something already built for the host, and skips the checks that need your source.

## Running the host

The host is one long-running process, `oikade-host`, with one data directory, secrets as files, logs on stdout, and metrics and health over HTTP. Put `oikade-host` from a [release](https://github.com/harrymitchinson/oikade/releases), in `oikade-host-linux-<arch>.tar.gz`, at `/usr/local/bin/oikade-host`, make a user for it that nobody logs in as, and run it under systemd:

```sh
sudo useradd --system --home-dir /var/lib/oikade --shell /usr/sbin/nologin oikade
```

```ini
# /etc/systemd/system/oikade.service
[Unit]
Description=Oikade host
After=network-online.target
Wants=network-online.target

[Service]
User=oikade
ExecStart=/usr/local/bin/oikade-host --data-dir /var/lib/oikade --api-addr :8443
StateDirectory=oikade
StateDirectoryMode=0700
Restart=always
RestartSec=2
KillMode=mixed
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now oikade
```

Add any other flags for `oikade-host`, such as `--broker`, to `ExecStart`. `--api-addr` serves the [network API](#the-network-api), which is how you reach the host from another machine. That machine needs only `oikade-host`. If you put `oikade` there too, its commands use the socket in the data directory, which only the host's user can reach, so run them with `sudo`. Your home and its plugins run as that user too, so give it nothing it doesn't need.

When the host stops, it goes in order: the API, ending streams such as `oikade tail`, then your automations, then the writes already under way, then each plugin, which is sent `SIGTERM` and says it's offline before its socket closes, then the Matter bridge and the store. The log has a line for each. `--stop-timeout` (10 seconds) bounds all of it: past that the rest is cut off, the log says where, and the host kills what's left and exits with an error. No state that must survive is lost, because the host only ever crashes safe, though `oikade replay` can miss the last few seconds. `KillMode=mixed` is what leaves the order to the host: by default systemd sends `SIGTERM` to your automations and plugins at the same moment as the host.

### In Docker

The image holds only `oikade-host`, for linux/arm64 and linux/amd64, and runs it. Matter needs the machine's network, for mDNS and IPv6:

```sh
docker run -d --name oikade --restart unless-stopped --stop-timeout 15 --network host \
  -v oikade:/var/lib/oikade ghcr.io/harrymitchinson/oikade:<version> --api-addr :8443 --broker mqtt://broker:1883
docker exec oikade oikade-host client add laptop
```

`--stop-timeout 15`, before the image, is Docker's: it gives the host longer than its own 10 seconds to stop before Docker kills it. The flags after the image are `oikade-host`'s. `client add` prints the `oikade login` line to run on your laptop, and from then on you use the container from there, over the [network API](#the-network-api). To upgrade, run `oikade backup`, then replace the container on the same volume.

### Secrets

```sh
oikade secret set api-key         # asks for the value, or reads it from stdin
oikade secret list                # names only, with --json too
oikade secret remove api-key
```

Values are never in logs or `--json`, and never on a command line, where `ps` and your shell's history would keep them. Each secret is a file in `secrets` in the data directory, readable only by the host's user, so a backup has them. Your code reads one with `framework.Secret`, and a plugin with `plugin.ReadSecret`, when it next asks. `oikade secret set` restarts nothing, so code that reads a secret once at start sees the new value after your next deploy.

`--secrets-dir DIR` on `oikade-host` points at another directory, which can be read-only, such as a systemd credentials directory. `oikade secret list` shows what's in it, and `set` says to change it where it's mounted from.

A broker's password belongs in a secret, not a flag: use `--broker mqtt://oikade@broker:1883 --broker-password-secret broker`. The host never logs one.

### Upgrading the host

```sh
oikade backup                                                             # a snapshot of the store, in backups/
sudo cp -p /usr/local/bin/oikade-host /usr/local/bin/oikade-host.previous # to go back to
sudo install -m 755 oikade-host /usr/local/bin/oikade-host                # the new version, as a new file
sudo systemctl restart oikade
```

Unlike a deploy, this restarts the host, so Matter, your automations and your plugins all stop until it's back, and the Home app shows the bridge as not responding until it reconnects. The new `oikade-host` opens the same data directory, so nothing needs pairing again. If it doesn't come up, `sudo install -m 755 /usr/local/bin/oikade-host.previous /usr/local/bin/oikade-host` and restart. Update `oikade` on the machines you run commands from to the same version, as an older or newer client may not speak this host's API.

A new host may speak a new protocol to your code, in which case your process is turned away until you bump `oikade` in your module and deploy. Do that straight after upgrading.

## Working with a running home

These talk to the host through a Unix socket in its data directory, which only the user running the host can reach. The [host API](docs/host-api.md) is also served over TLS if you ask for it, with a [token](#the-network-api).

```sh
oikade status                     # the running version, every device, and your rules
oikade status valve               # one device
oikade tail                       # changes, writes, refusals and rule runs as they happen
oikade set fan on off      # turn it off
oikade pause fan                  # stop a rule
oikade resume fan
```

`oikade status` says why as well as what: an arbitrated capability shows its winning claim, such as `Rule/leak-shutoff wants false at Safety`. With your automations down it says since when, and with nothing deployed it says to run `oikade deploy`. An empty `status`, `devices`, `secret list` or `client list` says what would fill it, on stderr, and `--json` prints an empty list. A hold says whether it's active and why it isn't sure, such as `lounge-lamp  hold  active since 23:07, off at 23:10`, or `hall-heater  hold  inactive  stopped: no device in room hall has reported occupancy`. A time that isn't today has its date, and an on or off time that has gone by says `(passed)`. A hold whose `When` can't be read keeps its state until it can. A budget that can't be planned keeps its device off.

`oikade tail` says `Following home's events` on stderr once the host is listening, naming the context, so a quiet house doesn't look hung.

`oikade set` acts like tapping the device. An arbitrated capability treats it as an outside change, shown as `Command`, so its `external` and `overrideHold` settings decide how long it lasts. A capability a Safety claim holds refuses it, rather than writing it back a moment later. It waits up to 3 seconds for the device to answer. A report within 0.5 of the value is its answer, as a driver rounds brightness 33 to 33.1, but a report of another value on the way, as from a lamp mid-fade, isn't. Then it says what the capability ended up at and why, such as `towel-rail on is false, not true: nothing claims it, so its Default wins` when `external: Revert` wrote it back, or that the device hasn't answered yet.

A pause is state, like tapping a light: it's in the store, needs no Git and lasts through deploys and restarts until `oikade resume`. A paused rule stops at once: moments and flows don't start, a flow's run in progress ends, and a hold stops working out its target and leaves its device as it is. Its claims end, except Safety ones, which end only with a release, so a pause can't reopen a valve. Times that pass during a pause aren't caught up. Pausing a Safety rule stops that protection, so it needs `--allow-safety`. So does pausing a rule holding a Safety claim, and anything while no process is connected and no deployed version says what it is. A Safety claim made by a flow you paused stays until something releases it, because the run that would have is gone. A rule deleted by a deploy loses its pause. `oikade pause` names the claims it ended, and the Safety ones it kept.

`--json` on `status`, `tail`, `replay`, `set`, `pause`, `resume`, `devices` and `matter` prints JSON whose fields stay stable for scripts.

From another machine, these and `deploy` and `rollback` go over the [network API](#the-network-api), once you've logged in.

### The network API

`oikade-host --api-addr :8443` also serves the API over TLS, for clients with a token. It's off without the flag, and an address with no port uses 8443. If the port is taken, the host logs that and carries on without it, and a restart tries again. Its first start makes a key and a self-signed certificate, kept in the store, and the log line `Serving API over TLS` gives its `pin`: the SHA-256 of its public key, which a client checks instead of a name or an expiry, so it works by IP address.

To use it from your laptop, on the host's machine run:

```sh
sudo -u oikade oikade-host client add laptop
```

It prints a line to run on the laptop within 10 minutes:

```sh
oikade login pi.home:8443 --pin sha256:9f2c… --code k7Qx…
```

The pin is in the line, so the laptop trusts nothing it hasn't been told. It checks the pin before it sends the code, and the code works once. The login saves the host as a context in `~/.config/oikade/config.json`, with mode 0600, and makes it current. From then on `oikade status`, `deploy` and the rest go to it. `--context NAME` picks another, and `--data-dir` means the host on this machine. `oikade-host client add` always means the host on its own machine, through the socket, so it has no `--context`. A config file others can read is refused.

CI has no one to type a code, so give it a token instead:

```sh
sudo -u oikade oikade-host client add ci --token     # on the host's machine: prints a token and the pin
```

Keep the token in a CI secret as `OIKADE_TOKEN`, then run `oikade login ADDRESS --pin PIN` with it set. That saves a context with no token in it, and `OIKADE_TOKEN` supplies one whenever that context is used. A context with a token of its own never sends `OIKADE_TOKEN`, so a token meant for one host doesn't reach another.

```sh
oikade client list               # who has a token, and when each last used it
oikade client revoke laptop      # its token stops working at once
```

`client add` works only on the host's machine, through the socket, so a stolen token can't make more of them. `revoke` works over the network API too, so any client can cut off a lost one, and it ends that client's open streams. It works while the listener is down, so a token can't come back when the port frees. The token is shown once, and the store keeps only its hash. Every token can do everything: a deploy runs code as the host's user, so **a token is as good as a login on the host.** It keeps a stranger on your network out. It isn't for the open internet, so reach the host over your own VPN. The host logs which client made each call.

## Metrics

The host serves these on `:8080/metrics`, and health on `:8080/healthz`, over HTTP. That listener is read-only and apart from the local API. It's open to whoever can reach the port, and it shows device names and values. `--metrics-addr` moves it, such as to `127.0.0.1:8080`, and an empty value turns it off. If the port is taken the host logs that and carries on without it. Your process's rule metrics reach the host every 5 seconds, so a scrape can be that far behind, and they go when the process does. `oikade_undeclared_devices` and the plugin metrics have no series on the host yet. With Matter off (`--matter-port=0`) there are no `oikade_matter_*` series.

Point Prometheus at it, and import [`config/grafana/oikade-home.json`](config/grafana/oikade-home.json) into Grafana for the "Oikade home" dashboard.

| Metric | |
|---|---|
| `oikade_capability_value{device,room,capability}` | Current value. Bools are 0 or 1. |
| `oikade_capability_changes_total{…}` | Counts changes, including ones shorter than the scrape interval (a door opened and closed within 15s) |
| `oikade_device_available{device,room}` | 1 online, 0 offline |
| `oikade_device_last_seen_timestamp_seconds{…}` | |
| `oikade_undeclared_devices{instance}` | Paired but not on record |
| `oikade_plugin_up{plugin}` | 1 while the plugin reports itself online with contract 1 |
| `oikade_plugin_rejected_messages_total{plugin,reason}` | Plugin messages dropped as `malformed`, `wrong_contract`, `unknown_capability`, `invalid_value`, `undeclared_device`, `unannounced_capability` or `unknown_device_type` |
| `oikade_rule_runs_total{rule,result}` | `succeeded`, `failed`, `cancelled`, `busy`, `condition_false`, `condition_error`, `missed` |
| `oikade_rule_next_change_timestamp_seconds{rule}` | When a rule's next time of day fires, or its hold next starts or ends |
| `oikade_rule_active{rule}` | 1 while a Rule's hold is active, 0 otherwise |
| `oikade_rule_budget_target_minutes{rule}` | A budget hold's minutes for the current planning day |
| `oikade_rule_budget_scheduled_minutes{rule}` | How many of them today's plan places |
| `oikade_rule_budget_run_minutes_total{rule}` | Minutes a budget hold's device has been on and available |
| `oikade_rule_budget_today_run_minutes{rule}` | Those minutes in the current planning day |
| `oikade_rule_budget_desired{rule}` | 1 while a budget hold's plan wants its device on, 0 otherwise |
| `oikade_device_control_level{device,capability}` | The level that wins an arbitrated capability: 0 default, 1 Normal, 2 Override, 3 Safety |
| `oikade_rule_events_dropped_total` | Capability changes the rule engine dropped because it fell behind |
| `oikade_api_auth_failures_total` | Calls to the network API refused for want of a valid token |
| `oikade_matter_serving` | 1 while the Matter bridge serves controllers, 0 while it can't. `oikade matter` says why |
| `oikade_matter_fabrics` | Fabrics the bridge is paired on, one for each controller's home |
| `oikade_matter_sessions` | CASE sessions that paired controllers hold |
| `oikade_matter_subscriptions` | Subscriptions that controllers hold |
| `oikade_matter_reports_total` | Reports sent on those subscriptions, the first one and keep-alives included |
| `oikade_matter_interactions_total{interaction,cluster,result}` | Attribute `write`s and command `invoke`s from controllers, by cluster (`on_off`, `level_control`, …, or `other`, which includes a whole request refused before any cluster was looked at) and the status they were answered with (`success`, `unsupported_access`, …) |
| `oikade_matter_panics_total{site}` | Panics in handling one Matter message, which cost that message and its session rather than the host, by `site`: `receive`, `handler`, `subscription` or `mdns`. Each is a bug, and the log has its stack. A panic anywhere else in Matter, such as a timer or a commissioning command, still stops the host |
| `oikade_matter_write_duration_seconds` | Histogram of how long a controller's change takes to get through arbitration to its device's driver, for changes that worked. Writes gather for 50 ms first, which this leaves out |

Every capability change is also logged as `"Changed capability"` with structured fields, so Loki has the event history.

## Matter bridge

Oikade reaches the Home app and other controllers through Matter ([OIK-76](https://linear.app/oikade/issue/OIK-76)). The bridge runs inside `oikade-host`, so nothing your code does reaches a controller: a deploy, a crash or a stuck process leaves sessions up and devices working ([OIK-143](https://linear.app/oikade/issue/OIK-143)). A host crash does take the bridge down, until it restarts.

```sh
oikade matter    # the pairing codes, whether it's ready to pair, and what it's paired with
```

The host needs host networking on its machine: the same L2 network as your controllers, with mDNS and IPv6. `--matter-port` moves the bridge from UDP 5540, and 0 turns Matter off. `--matter-name` names it, and `--matter-interfaces` limits where it advertises. If its port is taken, or an interface isn't there, the host logs that and carries on without it, tries again every minute, and `oikade matter` says it's down and why: automations don't need it. While it's on no fabric, the commissioning window stays open for 15 minutes after the host starts. Restart the host to open it again.

Every device on record with something Matter can show is on the bridge, at its [registry](#devices) endpoint, as soon as it's paired, as its [device types](#what-a-device-is). Its label is its name, and a rename changes that in place: the endpoint and unique ID stay, so a controller keeps its room and scenes. A new device type rebuilds the device on the same endpoint, with the same unique ID. Removing a device takes it off. A device whose driver hasn't reported yet stays on, unreachable, so controllers don't see it vanish.

A controller's write goes through [arbitration](#arbitration) in the host, shown as `Matter`, and doesn't need your process. A capability a Safety claim holds refuses it, including a claim still on disk from before a restart: the write never reaches the device, the controller sees the value go back, and the log says who holds it. A device whose claim record can't be read counts as held at Safety: nothing writes it, nothing writes over the record, and the log says so. `oikade devices remove --allow-safety` clears it.

Add the bridge to a controller, such as the Home app, with the 11-digit manual code, or a QR code of the `MT:` payload. In the Home app, choose **Add Accessory**, then **More options**, then the bridge, and enter the code. Apple Home warns that it's an uncertified accessory: choose Add Anyway. It needs a home hub. To add the bridge to a second controller, open a window from the first one (in the Home app: the bridge's settings, Turn On Pairing Mode).

- **Writes:** a burst of commands from a controller, such as a swipe in the Home app, is gathered for 50 ms per device and capability, and only the last is written, unless the device is already in that state.
- **Unsupported devices:** a device none of whose capabilities map to Matter, such as one with only `link-quality`, keeps its endpoint ID but isn't on the bridge.

## Devices

The host owns the list of devices, in its registry in the data directory. A paired device enters it the moment a driver reports it, and it's usable at once, under a name copied from its app once. Oikade never writes to an app's names, or follows them after that.

```sh
oikade devices                               # every device, and how it's doing
oikade devices name lamp-2 porch-light       # call it something else
oikade devices room porch-light Porch        # move it between rooms
oikade devices type porch-light OnOffLight   # say what it is
oikade devices reserve shed-heater           # hold a name for hardware you haven't paired yet
oikade devices replace porch-light lamp-3    # move a name onto new hardware
oikade devices remove porch-light
oikade devices remove --plugin relays        # every device of a plugin you no longer import
oikade types                                 # write devices.go from the registry
```

`oikade types` writes a Go file with a value for each device, with its name and a handle for each of its capabilities, such as `PorchLight.On`, and you commit it. So code naming a device that's gone, or a capability it no longer has, stops compiling, builds need no host, and Git has a reviewable copy of your inventory. `--out` and `--package` say where it goes. The registry is the authority, and the file is its snapshot. Two names that come out the same in Go, such as `a-1` and `a1`, fail until you rename one. Running code loses track of a renamed device until you deploy, so run `oikade types` and deploy straight after. While a Safety rule runs, `name` and `replace` need `--allow-safety`, because a Safety rule that names the device stops protecting anything until that deploy.

`reserve` lets you write code for something that isn't paired yet, which does nothing until the hardware arrives. A reserved device has no capabilities yet, so `oikade types` gives it only its `Name`: until you pair it, name a capability with a handle of your own, such as `framework.Handle[bool]{Device: ShedHeater.Name, Capability: framework.On}`, and run `oikade types` once it's paired. Pair it, and `oikade devices replace shed-heater <its name>` moves the reserved name onto it. `replace` is also how a failed device is swapped: the name, room, claims and Matter endpoint stay, so the Home app keeps its rooms and automations. The new hardware's own entry goes, and the old hardware counts as new if it's ever paired again.

`remove` keeps the device on record with its endpoint, so hardware that comes back gets its name, room and endpoint back. Remove a device from its app first, because one that's still paired comes back when it next reports. A device stays until you remove it, reported or not. Renaming a device, moving its hardware onto another name, or restoring a file that would move its name, is refused while something claims it, because claims stay with a name. Removing a device ends its claims, and one that a Safety claim holds needs `--allow-safety`. `oikade devices` shows a device as `plugin not imported` when the running version doesn't import its plugin, and `oikade status` marks it the same way (`pluginNotImported` in `--json`).

If the registry is lost, `oikade devices restore devices.go` reads the file back: names, rooms, device types, reservations, arbitration settings and endpoint IDs, matched by hardware. The registry is the authority, so a file that disagrees with one in use is refused, which is what an old file does. After a real loss the drivers may have reported again first, under their apps' names, so pass `--force` there and the file wins. With `--force`, devices the file doesn't have stay, and get a new endpoint or name if the file gives theirs to something else. A claimed name never moves.

## State that must survive

Losing these means re-pairing, and then the Home app forgets every room, scene and automation, or every Zigbee device has to be paired again.

- **The host's data directory** (`/var/lib/oikade` by default) is everything a home has to keep, in one file: the Matter bridge's setup code, unique ID, attestation key and fabrics, the device registry with its endpoint IDs, latches, claims, flows, your deployed versions, your secrets, and the network API's key and token hashes. **Back up one directory.** A file copied while it changes may not open, so either stop the host first, or run `oikade backup` and then copy the directory. `oikade backup` writes a consistent snapshot of the store to `backups/oikade-<time>.db` in the directory while the host runs, and keeps the last five. Secrets are in there in the clear, so keep the copy somewhere as private as the machine.
  - **Upgrade:** take that snapshot first with `oikade backup`, and a new `oikade-host` opens the same directory, so nothing needs pairing again. A file from a newer format is refused rather than guessed at, so keep the backup until the upgrade has worked.
  - **Delete:** deleting the directory forgets the bridge. Every controller has lost it, so remove it from each one and pair again. The next host gets new codes.
  - **Restore:** with the host stopped, copy the directory back, give it to the host's user, and if it came from a running host, copy the newest `backups/oikade-<time>.db` over `oikade.db`. A host on a restored directory is the same bridge, with no pairing again, because the identity and fabrics come back with it. A controller added since that backup isn't in it, and needs pairing again. If the Matter identity or fabrics are unreadable, the host never makes new ones: it runs the home without a bridge, and the log and `oikade matter` say why, until you restore them. A restore keeps the network API's pin and every token, including any revoked since that backup, so revoke those again.
- **The device registry** is in the data directory, and holds each device's name, room, device type and Matter endpoint ID. Endpoint IDs are never reused or reordered, and removed devices keep theirs. Back up the directory, or commit `oikade types`' file. Restoring from that file brings back everything it had: endpoints handed out after you last ran `oikade types` aren't in it, so run it after pairing.
- **The Zigbee network key and database are yours,** because they belong to the Zigbee2MQTT you run. They aren't in Oikade's data directory, so back up Zigbee2MQTT's own data directory as well. Oikade's backup doesn't cover it. Losing it means pairing every device again.

## Security notes

- The API is a Unix socket only the host's user can reach. With `--api-addr` it's on the network too, over TLS 1.3, and every call needs a token, which is as good as a login on the host. Keep it off the open internet.
- The metrics listener is read-only, but open to whoever can reach the port, and it shows device names and values. Move it with `--metrics-addr`.
- Importing a plugin means trusting its code, because it runs as the host's user.
- An outside plugin and Zigbee2MQTT are as safe as your broker: anything that can publish to it can control those devices. Give it a password, and keep it in a [secret](#secrets).
- Each plugin's README says what else it needs, such as the [eWeLink LAN plugin](plugins/ewelinklan/README.md#security).

## Logging

The host writes structured JSON logs to standard output, and your process and plugins log to standard error. Under systemd, `journalctl -u oikade` has them all. Every capability change is logged as `"Changed capability"` with structured fields.

## Versioning

Releases follow [semantic versioning](https://semver.org), and Git tags and image tags use the same version, for example `v0.1.0`. Oikade's public API is `framework`, the `oikade` and `oikade-host` commands and their `--json`, the plugin contract and SDK, capability names, metric names and labels, and Matter endpoint IDs.

While the major version is 0, the API is still settling, and versions follow the milestones, which are the releases in the plan (`docs/planning.md`):

- Releases on the way to a milestone are pre-releases of it: `v0.1.0-alpha.1`, `v0.1.0-alpha.2`, … Any of them may break the public API, so read the release notes before upgrading.
- Shipping a milestone cuts its version (`v0.1.0`). Releases after it are `v0.2.0-alpha.N`, on the way to the next one.
- No commit moves MINOR or MAJOR on its own, breaking changes included.

`1.0.0` is a deliberate decision, not something a commit triggers.

Releases are cut by merging the release PR that [release-please](https://github.com/googleapis/release-please) keeps open on `main`. That gives the tag and a GitHub Release with notes. [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) then adds `oikade` for darwin and linux, `oikade-host` for linux, each on arm64 and amd64 as a `.tar.gz`, and `checksums.txt`, and pushes the image `ghcr.io/harrymitchinson/oikade:<tag>` for arm64 and amd64. `make release-snapshot` builds all of it into `dist/` without publishing, and every PR that could change it runs that. CI doesn't run on the release PR, because it's opened with `GITHUB_TOKEN`. That's fine, since it only changes `CHANGELOG.md` and `.release-please-manifest.json`. release-please needs **Settings → Actions → General → Allow GitHub Actions to create and approve pull requests** turned on. The first release creates the ghcr package as private, so set its visibility to public once, in the package's settings.

To ship a milestone:

1. Merge a commit whose message ends with a `Release-As: 0.1.0` footer, as its last paragraph. With a squash merge, put it last in the squashed message.
2. Merge the release PR for `v0.1.0`.
3. Before anything else merges, merge a commit with a `Release-As: 0.2.0-alpha.1` footer. Otherwise release-please's prerelease versioning proposes `v0.2.0-alpha` after a feature, or `v0.1.1-alpha` after a fix, rather than the next milestone's first alpha.

## Development

```sh
make test               # unit and integration tests, with a real host and simulated devices
make lint
make test-conformance   # builds the simswitch test fixture and drives it through the plugin contract and the Go API
make test-journeys      # from oikade init to a running host, and pairing chip-tool with it (test/journeys/chip-tool, linux/arm64 only)
```

The plugin SDK, the conformance suite and the simswitch fixture are Go, in the same module, so `make test` and `make lint` cover them. How the host, rule engine and Matter bridge work inside is in [docs/architecture.md](docs/architecture.md).

## Not yet

Planned work is tracked in [Linear](https://linear.app/oikade). Each release is a project there, and its description gives its goal, exit criteria and why it sits where it does. Designs live in their features' issues there, and [docs/planning.md](docs/planning.md) says how planning works.

## Not doing

Oikade isn't a general-purpose hub. It has no Home Assistant driver and no UI for configuration.

- **Home Assistant as a driver.** Standalone device libraries that Home Assistant also uses are fine inside plugins.
- **Kubernetes.** The host is one process with one data directory, on systemd or in Docker.
- **Replicas or leader election.** One host per home is deliberate, because two would run every automation twice.
- **Homebridge as a driver.** It would make Homebridge the source of device identity ([OIK-61](https://linear.app/oikade/issue/OIK-61)).
- **Go `plugin` or WASM plugins.** The first needs CGO, and neither can do LAN multicast usefully.
- **Configuration from a UI.** Your automations are code, and the Zigbee2MQTT web UI is yours.
- **Device types the Home app can't use over Matter.** On iOS 27.2, valves, garage doors and other closures, plain doorbells and TVs show as an icon with no controls, and Matter has no security system type ([OIK-101](https://linear.app/oikade/issue/OIK-101)).

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
