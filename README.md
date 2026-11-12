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

`init` writes a module with one automation, its test, a `devices.go` with two example devices, and a GitHub Actions workflow. The workflow runs `oikade check` and `oikade test` on every change, and `oikade deploy` once one reaches `main`, over the [network API](docs/running-a-home.md#the-network-api). It needs the secret `OIKADE_TOKEN`, from `oikade-host client add ci --token`, and the variables `OIKADE_ADDRESS`, such as `pi.home:8443`, and `OIKADE_PIN`, which that command prints. GitHub's own runners can reach the host only if its API is on the internet, which it shouldn't be, so run the job on a self-hosted runner on your network, or join the runner to your network over a VPN first. `oikade types` replaces `devices.go` once you have real devices.

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

`devices.go` has a value for each device: `PorchLight.Name` is its name, and `PorchLight.On` is a handle on one of its capabilities, typed `bool` or `float64`. Your editor lists what each device can do, and `s.Get` reads a handle, `To` writes one, and a `Change` is one changing. The capabilities are constants in `framework`, such as `framework.On`, for [rooms](docs/automations.md#rooms).

`oikade check` finds what's wrong before anything runs: the compiler, so a misspelt device, a capability the device doesn't have or a value of the wrong type doesn't build, then [flows that wouldn't replay](docs/writing-flows.md) and a rule's chained method whose result isn't used, then the checks a deploy makes, which include an option given twice. `oikade deploy` makes these checks too. If it can reach your host, it also lists the devices a deploy would leave behind because their [plugin](docs/running-a-home.md#removing-a-plugin) is no longer imported.

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

Arbitration and budgets are parts of the same home, and a [`Bundle`](docs/automations.md#composing-a-home) keeps them together. `Control` is [arbitration](docs/automations.md#arbitration), and a `Hold` with a `Budget` is [a budget](docs/automations.md#budgets):

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

## Plugins

A plugin connects a device platform that isn't Zigbee, such as the eWeLink LAN protocol or the Overkiz cloud API, without changing Oikade. It targets the platform, not one device: a new device on that platform is a model in the plugin, not a new plugin. Devices it serves are ordinary devices: they show up on the Matter bridge, in Grafana and in rules like Zigbee ones.

On a host, a plugin is a Go module your home imports, as a `framework.Plugin` part of your home, and the host runs it as a process of its own, so one that panics or hangs is restarted while automations and Matter carry on. It runs as the host's user, so **importing a plugin means trusting its code**. A platform with no good Go library runs as an outside process speaking the contract to a broker of yours, with `oikade-host --broker URL --outside-plugins NAME`. The [contract](docs/plugin-contract.md#a-plugin-your-home-imports) says how.

First-party plugins, each with its own README:

- [Zigbee2MQTT](plugins/zigbee2mqtt/README.md): Zigbee devices, through a Zigbee2MQTT and an MQTT broker you run
- [eWeLink LAN](plugins/ewelinklan/README.md): relays with sensors on the eWeLink LAN protocol, such as SONOFF's TH Elite

To write a plugin in Go, use the [SDK](sdk/plugin) and the contract types in [`contract/v1`](contract/v1). A plugin's whole `main` is one `plugin.Run` call, and the SDK handles the Last Will, re-announcing after a reconnect, `/set` and SIGTERM. First-party plugins live in [`plugins/`](plugins), and anyone else's in its own repo, `oikade-plugin-<name>`, with its own images and releases. [`test/fixtures/simswitch`](test/fixtures/simswitch) is a complete plugin with simulated relays, which this repo's conformance tests drive. A plugin in another language implements the [contract](docs/plugin-contract.md) itself. Any plugin repo can check itself against the contract with [`oikade-conformance`](docs/plugin-contract.md#conformance).

## Guides

- [Automations](docs/automations.md): moments, holds, rooms, budgets, arbitration, and composing a home
- [Writing flows](docs/writing-flows.md): automations as ordinary Go that survive a crash
- [Devices](docs/devices.md): names, rooms, device types, capabilities and the Matter bridge
- [Running a home](docs/running-a-home.md): deploying, the host, secrets, upgrades, the network API, backups and security
- [Metrics and logs](docs/metrics.md): Prometheus, the Grafana dashboard and logging
- [Raspberry Pi](docs/deploy-raspberry-pi.md): a host on a Pi, from scratch
- Plugins: [Zigbee2MQTT](plugins/zigbee2mqtt/README.md) and [eWeLink LAN](plugins/ewelinklan/README.md)
- For contributors: [architecture](docs/architecture.md), the [plugin contract](docs/plugin-contract.md), the [host API](docs/host-api.md), [planning](docs/planning.md) and [contributing](CONTRIBUTING.md)

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
