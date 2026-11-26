# Testing a home

Four ways to find out what your home will do before it does it. `oikade check` finds mistakes, `oikade test` runs your tests on a fake clock, and `oikade dev` lets you try your home by hand. None of them needs a device or a host. `oikade replay` runs your working copy against what your host saw last week.

## Checking

`oikade check` finds what's wrong before anything runs, in three passes:

1. **The compiler.** A misspelt device, a capability the device doesn't have, or a value of the wrong type doesn't build, because each [device in your code](devices.md#devices-in-your-code) has a typed handle for each of its capabilities.
2. **The source checks.** These find a flow that [wouldn't replay](writing-flows.md#what-a-crash-does) after a crash, and an `oikade` value that's built and thrown away: a `Set`, a condition, or a chained method whose result isn't used, such as `HallLamp.On.To(true)` without `r.Set`.
3. **Your home's own checks,** which a deploy makes too. These are the mistakes [Automations](automations.md) and [Devices](devices.md) say it refuses, such as a capability decided twice, a time that isn't one, or a name a device can't have.

```console
$ oikade check
Your home is fine: 12 rules, 2 plugins
```

`oikade check` gives your code [your values](running-a-home.md#your-values), from `.env` and the environment, so what it builds from them is as it will be on the host. A value your code uses that nothing gives is only a warning, so a pull request passes without `.env`:

```console
$ oikade check
Warning: TAHOMA_PIN isn't set, so a deploy from here would stop
Hint: add it to .env, or set it in the environment
Your home is fine: 12 rules, 2 plugins
```

If it can reach your host, it also checks your code against the devices on record. It lists the devices a deploy would leave behind because their [plugin is no longer imported](running-a-home.md#removing-a-plugin), the ones it would [take off Matter](devices.md#hiding-a-plugins-devices-from-matter), and declared hardware that hasn't reported. It fails on a `ShowOnMatter` name that isn't one of that plugin's devices.

## Tests

`oikade test` is `go test`, for `./...` unless you name packages, and anything after them is `go test`'s too: `oikade test ./... -run TestNight`.

The `oikadetest` package runs the real engine, arbiters and planner in your test, with simulated devices and a fake clock. Set device values, move the clock, and look at what your automations did:

```go
func TestPorchLightFollowsThePorchSensor(t *testing.T) {
	h := oikadetest.New(t, home(), oikadetest.Options{})

	h.Set(PorchSensor.Occupied, true)
	h.ExpectValue(PorchLight.On, true)

	h.Set(PorchSensor.Occupied, false)
	h.Advance(4 * time.Minute)
	h.ExpectValue(PorchLight.On, true)

	h.Advance(2 * time.Minute)
	h.ExpectValue(PorchLight.On, false)
}
```

| Method | |
|---|---|
| `h.Set(c, v)` | Changes a device from outside your automations, as its sensor or a tap in an app would |
| `h.Report(sets…)` | Changes several values of one device at once, as one message from it would |
| `h.Advance(d)` | Moves the clock on |
| `h.Restart()` | Crashes your automations and starts them again |
| `h.ExpectValue(c, v)` | Fails the test unless a device has this value |
| `h.ExpectWrite(c, v)` | Fails the test unless your automations have written this value to a device |
| `h.ExpectClaim(c, source, level)` | Fails the test unless `source`, a ref such as `case/simulator/porch-light/on/motion`, holds a claim at this level |
| `h.ExpectFinished(flow)` | Fails the test unless a flow has run to the end |
| `h.Value(c)`, `h.Writes()`, `h.Claims(c)`, `h.Finished(flow)`, `h.Now()` | Read the same, for a test that checks for itself |

Each change waits for your automations to take it in, so the next line sees what they did. A mistake in a condition goes to the test's log, which `go test` shows when the test fails.

### A crash in the middle of a flow

A restart is one call, and a flow carries on from where it had got to:

```go
func TestBedtimeSurvivesACrash(t *testing.T) {
	h := oikadetest.New(t, home(), oikadetest.Options{})
	h.Advance(22*time.Hour + 40*time.Minute) // Into the flow's 30-minute wait.
	h.Restart()
	h.Advance(21 * time.Minute)
	h.ExpectValue(Lounge.On, false)
	h.ExpectFinished("bedtime")
}
```

### A report of several values

A sensor often reports several values at once, and your rules see all of them before any acts, so a safety line on one is never late for a lower line on another. `h.Report` sends one device's values as one report, as the host does:

```go
h.Report(Hall.Humidity.To(72), Hall.Occupied.To(true))
h.ExpectValue(HallFan.On, true)
```

### Options

| Option | |
|---|---|
| `Start` | When the test starts. Zero is midnight UTC on Monday 7 January 2030 |
| `Devices` | The devices, as `oikadetest.Device`s. Empty is the ones your home declares with `oikade.Device` |

Under `go test` and `oikadetest`, an `oikade.Var` is empty.

## Trying it by hand

`oikade dev` runs your automations against a simulated home of the devices your code declares, on a clock that starts now and moves when you say:

```text
$ oikade dev
Simulating 2 devices from Sat 11:00. Type help for the commands.
Sat 11:00 Wrote hardware/simulator/porch-light/on = false
set hardware/simulator/porch-sensor/occupied true
Sat 11:00 case/simulator/porch-light/on/motion applies
Sat 11:00 Wrote hardware/simulator/porch-light/on = true
set hardware/simulator/porch-sensor/occupied false
advance 6m
Sat 11:05 case/simulator/porch-light/on/motion no longer applies
Sat 11:05 Wrote hardware/simulator/porch-light/on = false
```

| Command | |
|---|---|
| `set REF VALUE` | Changes a device from outside, as a sensor or an app would: `REF` is the capability's, as `oikade set` takes it. `true`, `on`, `false` and `off` are bools, and a number is a number |
| `advance DURATION` | Moves the clock on, as in `advance 90m` |
| `status` | Shows every device's values, and what holds each |
| `restart` | Crashes your automations, and starts them again |
| `help`, `quit` | |

- **The devices** are the ones your home [declares with `oikade.Device`](devices.md#declaring-devices-in-code), in their rooms, of their types and with the capabilities their lines give them. A device in no room is in none. None has a value until you set it. Hardware declared with `home.Device` has no capabilities to simulate, so `oikade dev`, `oikade replay` and `oikadetest` refuse it: `oikade types` gives it a line.
- **Your values** come from `.env` and the environment, or the file `--env-file` names, as on the host. `oikade dev` doesn't run plugins, so nothing logs in.
- **Your automations' log** goes to standard error, beside the conversation on standard output, so `oikade dev 2>dev.log` keeps it apart.

## Replaying last week

`oikade replay` shows what your code would have done last week. It fetches the host's event log, runs the code in your working copy against it on a fake clock, and compares that with what the deployed version did:

```text
$ oikade replay
Replayed Fri 30 Oct 19:02:11 to Fri 6 Nov 19:02:11: 161240 changes. The deployed version wrote 412 times, your code 414.
- Mon 2 Nov 07:12:40 bathroom-fan on = true
+ Mon 2 Nov 07:04:10 bathroom-fan on = true
```

A `-` is a write the deployed version made and your code doesn't, and a `+` the reverse. Two writes within 5 seconds count as the same one. Unchanged code shows `No differences.`

- **`--since 48h`** compares a shorter time, 7 days by default. Your code always runs from the start of the log, so that it gets there in the right state.
- **`--json`** prints the report.
- **It takes a while:** a week of 40 busy devices took about a minute on a laptop, so a full 14-day log takes about two.

A replay never reaches a device. Your code runs in a simulated home, as in `oikade dev`, and asks the host only for its status and its log.

- **A flow's `Do` isn't called.** It gets the answer the deployed version got, or none if that call isn't on record, and the report says so.
- **What it can't know comes as a note:** a pause, a deploy, or your automations being down. It also runs a rule the deployed version had paused.
- **Some things it can't see at all.** A device that went quiet counts as alive, and a host that died outright leaves no note.
- **Anything your code prints** goes to standard error, with its errors.

The log's length is the host's: [How far back](working-with-a-home.md#how-far-back) says how to change it.
