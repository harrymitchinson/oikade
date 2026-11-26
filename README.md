# Oikade

Home automation as code you can test, on one small machine.

Your home is a Go module of decisions and flows. It compiles, runs in tests on a fake clock, and deploys without stopping anything. `oikade-host` is one binary that keeps your devices, runs a Matter bridge for the Home app and other controllers, and runs your code. `oikade` is the command you run on your laptop or in CI.

## How it fits together

```text
 plugins, each a process      oikade-host                     around it
┌───────────────────┐     ┌─────────────────────────┐     ┌──────────────────────────────┐
│ Zigbee2MQTT       │     │ devices and their state │◀───▶│ your automations: a process  │
│ eWeLink LAN       │◀───▶│ arbitration             │     │ the host runs and replaces   │
│ Overkiz           │     │ the store               │     ├──────────────────────────────┤
│ Toshiba AC        │     │ the Matter bridge       │◀───▶│ the Home app, or any other   │
│ one of your own   │     │ the API                 │     │ Matter controller            │
│                   │     │ metrics                 │     ├──────────────────────────────┤
│                   │     │                         │◀───▶│ oikade, on a laptop or in CI │
│                   │     │                         │     ├──────────────────────────────┤
│                   │     │                         │────▶│ Prometheus                   │
└───────────────────┘     └─────────────────────────┘     └──────────────────────────────┘
```

- **`oikade-host`** is long-lived, one per home. It owns everything that must survive: which devices exist, their rooms and Matter endpoints, claims, and Matter's pairing. It serves Matter, an API on a local socket and over TLS on the network, and metrics on `:8080`.
- **Your automations** are your module's binary. The host starts it, restarts it if it dies, and swaps it on a deploy, while Matter stays up and your devices stay as they are.
- **Plugins** connect device platforms. Each runs as a process of its own, so one that hangs is restarted while the rest carry on.
- **`oikade`** is the CLI: `init`, `test`, `dev`, `deploy`, `status` and the rest. It reaches the host over its API.

Your code lives in Git, and your home's state lives in the host. Toggling a light from your phone is state, so Git doesn't see it and a deploy doesn't touch it. Prometheus keeps the history.

## Requirements

- **Linux with systemd, or Docker,** on arm64 or amd64. A Raspberry Pi is plenty.
- **The same L2 network as your controllers, with IPv6.** Controllers find the bridge over mDNS and reach it over IPv6.
- **A Matter controller with a hub,** such as the Home app with an Apple TV or HomePod as its home hub.
- **For Zigbee,** a coordinator, and a Zigbee2MQTT and an MQTT broker you run. See the [Zigbee2MQTT plugin](plugins/zigbee2mqtt/README.md).
- **Go 1.27 or later,** wherever you build your home.

## First run

You need Go and `oikade`, and no device or host:

```sh
go install oikade.dev/cmd/oikade@main
oikade init home && cd home
oikade test
oikade dev
```

Or take `oikade` from a [release](https://github.com/harrymitchinson/oikade/releases): `oikade-<os>-<arch>.tar.gz` holds it for darwin or linux, on arm64 or amd64, and `checksums.txt` has each archive's SHA-256.

`oikade init` writes a Go module: `main.go` with one decision, its test in `home_test.go`, `devices.go` with two example devices, a `.gitignore`, and a GitHub Actions workflow that checks and tests every change. `oikade dev` runs your home against simulated devices, on a clock you move by hand:

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

## A decision and its test

This is the decision `oikade init` writes, in `main.go`:

```go
func home() *oikade.Home {
	home := oikade.NewHome(oikade.Options{})
	home.Decide(PorchLight.On).
		Case("motion", true, PorchSensor.Occupied).Linger(5 * time.Minute).
		Default(false)
	return home
}
```

A decision is everything that sets one capability, as a list read top to bottom: the first case that applies wins, and `Default` is what's left. Here the porch light is on while the sensor sees someone, and for 5 minutes after. `PorchLight` and `PorchSensor` come from `devices.go`, so a misspelt device or a capability it doesn't have doesn't compile.

`oikade test` is `go test`. The `oikadetest` package runs the real engine in your test, with simulated devices and a fake clock:

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

The guides go on from here, in order: more automations, how to test them, then a host to run them.

## Guides

1. [Automations](docs/automations.md): decisions, conditions, times of day, arbitration, budgets, groups and rooms
2. [Writing flows](docs/writing-flows.md): automations as ordinary Go that survive a crash
3. [Testing a home](docs/testing.md): `oikade check`, `oikadetest`, `oikade dev` and `oikade replay`
4. [Devices](docs/devices.md): refs, declaring devices in code, capabilities and the Matter bridge
5. [Running a home](docs/running-a-home.md): installing the host, deploying, your values, CI, upgrades and backups
6. [Working with a running home](docs/working-with-a-home.md): `status`, `history`, `tail`, `set` and `pause`
7. [A host on a Raspberry Pi](docs/deploy-raspberry-pi.md): from a bare Pi to a paired home, step by step
8. [Metrics and logs](docs/metrics.md): Prometheus, the Grafana dashboard and the host's log

For contributors: [architecture](docs/architecture.md), the [plugin contract](docs/plugin-contract.md), the [host API](docs/host-api.md), [planning](docs/planning.md) and [contributing](CONTRIBUTING.md).

## Plugins

A plugin connects a device platform, such as Zigbee or the eWeLink LAN protocol, without changing Oikade. It targets the platform, not one device, and the devices it serves are ordinary devices: on the Matter bridge, in metrics and in rules. Your home imports a plugin as Go code, and the host runs it as the host's user, so **importing a plugin means trusting its code**.

First-party plugins, each with its own README:

- [Zigbee2MQTT](plugins/zigbee2mqtt/README.md): Zigbee devices, through a Zigbee2MQTT and an MQTT broker you run
- [eWeLink LAN](plugins/ewelinklan/README.md): relays with sensors on the eWeLink LAN protocol, such as SONOFF's TH Elite
- [Overkiz](plugins/overkiz/README.md): Somfy io shutters and screens through a TaHoma hub's local API, and Atlantic, Thermor and Sauter towel radiators through Cozytouch's clouds
- [Toshiba Home AC Control](plugins/toshibaac/README.md): Toshiba air conditioners with a WiFi module, through Toshiba's cloud
- [Hue](plugins/hue/README.md): the lights and motion sensors on a Philips Hue bridge, through its API on your network

To write one, the [plugin contract](docs/plugin-contract.md) says what a plugin speaks, the [SDK](sdk/plugin) makes a Go plugin's `main` one `plugin.Run` call, and [`oikade-conformance`](docs/plugin-contract.md#conformance) checks a plugin repo against the contract. First-party plugins live in [`plugins/`](plugins), and anyone else's in its own repo, `oikade-plugin-<name>`.

## Not yet

Planned work is in `plan/`, a folder kept beside this checkout on the maintainer's machine. Each release is a file there, whose description gives its goal, exit criteria and why it sits where it does, and designs live in their features' issues. [Planning](docs/planning.md) says how it works.

## Not doing

Oikade isn't a general-purpose hub. It has no Home Assistant driver and no UI for configuration.

- **Home Assistant as a driver.** Standalone device libraries that Home Assistant also uses are fine inside plugins.
- **Kubernetes.** The host is one process with one data directory, on systemd or in Docker.
- **Replicas or leader election.** One host per home is deliberate, because two would run every automation twice.
- **Homebridge as a driver.** It would make Homebridge the source of device identity (OIK-61).
- **Go `plugin` or WASM plugins.** The first needs CGO, and neither can do LAN multicast usefully.
- **Configuration from a UI.** Your automations are code, and the Zigbee2MQTT web UI is yours.
- **Device types the Home app can't use over Matter.** On iOS 27.2, valves, garage doors and closures other than window coverings and locks, plain doorbells and TVs show as an icon with no controls, and Matter has no security system type (OIK-101).

## Licence

Licensed under the [Apache License, Version 2.0](LICENSE).
