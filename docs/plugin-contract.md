# The oikade/v1 plugin contract

A plugin is a separate process that connects a device platform Oikade doesn't speak natively (the eWeLink LAN protocol, the Overkiz cloud API) to the MQTT bus. It speaks **canonical capabilities**, not vendor payloads, so Oikade needs no code for the platform. The design is in [OIK-61](https://linear.app/oikade/issue/OIK-61).

This page is the contract. A plugin that follows it works with Oikade's runtime and Matter bridges unchanged.

On a host there are two ways to run one ([OIK-143](https://linear.app/oikade/issue/OIK-143)): [as a Go module your home imports](#a-plugin-your-home-imports), or as an outside process speaking this contract to a broker of yours.

## A plugin your home imports

A plugin is a Go module with a `plugin.Factory`. Your home imports it and registers it, and there's no image or binary to run:

```go
framework.Run(framework.NewHome(framework.Options{}, framework.Plugin{Name: "ewelink", New: ewelink.New}))
```

**Importing a plugin means trusting its code.** It runs as the host's user, with everything that user can reach.

The host runs your binary again as `<binary> plugin <name>`, one process per plugin, each with its own heartbeat. So a plugin that panics, or hangs, is restarted with backoff while automations, Matter and the other plugins carry on. A plugin hangs if its process stops answering, or a `Set` runs for longer than `--plugin-set-timeout` (30 seconds). Only the binary changing restarts a plugin that's working, so a deploy does, and a crash of your automations doesn't.

A `Handler` is the same either way: `Serve` reports devices, and `Set` applies a command. It gets the devices on record for it, which are empty the first time, because the host has no `devices.json`. So it reports every device it can see, with `SetDiscovered` and `SetDevices`, and everything it finds is in the [device registry](../README.md) at once. Configuration, such as a hub's address, is Go in your home.

A device you remove with `oikade devices remove` stays removed while its plugin keeps listing it. If it leaves the list and returns, as a re-paired device does, it's new.

While a plugin is down, its devices are offline and writes to them fail with an error, until its next process has reported them. A claim on one is written again as soon as it does, and a write its last process took with it is written again rather than read as somebody changing the device. Claims on them hold, in the host, and the arbiter puts a device right when it reports again. A home declares a plugin device's arbitration settings in `framework.Home.Control`, as for any device.

The messages are this contract's, on the host's socket rather than a broker, so the host checks them with the same decoder. A process that dies is marked offline by the host, as a Last Will would.

## An outside process

For a platform with no good Go library, a process of its own speaks this contract to a broker of yours. Name it to the host, and it uses the root `oikade/v1/<name>`:

```sh
oikade-host --broker mqtt://mosquitto:1883 --outside-plugins ewelink,overkiz
```

The host supervises none of this: the process, its restarts and its broker are yours. A broker that isn't up yet is tried until it is, and until then these plugins' devices aren't known and their claims aren't written. Devices it reports go in the registry like any others. It reads no `devices.json` from the host, so give it one of your own, or have it report what it can see.

## Topics

Every topic sits under the plugin's root, which it gets in `OIKADE_TOPIC`:

```
oikade/v1/<plugin>
```

`<plugin>` is the plugin's name, and `<id>` is a device's plugin-local ID. Both match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`, so neither contains `/`.

| Topic | Direction | Retained | Payload |
|---|---|---|---|
| `status` | plugin → Oikade | yes | `{"state":"online","contract":1}`. Set the MQTT Last Will to `{"state":"offline","contract":1}`, retained. |
| `devices` | plugin → Oikade | yes | The devices the plugin serves. See below. |
| `<id>/state` | plugin → Oikade | yes | The device's **full** current values, such as `{"on":true}`, leaving out only values a device has yet to report and can't be asked for. |
| `<id>/availability` | plugin → Oikade | yes | `online` or `offline`, as a bare string. |
| `<id>/set` | Oikade → plugin | no | One capability per message, such as `{"on":false}`. |

Retain everything the plugin publishes. The host rebuilds its picture from retained topics when it starts or reconnects, and it may receive those topics in any order.

Other topics under the root are reserved. Oikade ignores them.

### Oikade's own topics

Roots under `oikade/v1/` that start with `_` are Oikade's. Don't publish to them or depend on them.

### `status`

Publish `{"state":"online","contract":1}` once connected, and set the Last Will so that the broker publishes `{"state":"offline","contract":1}` if the plugin dies. While the status is offline, Oikade treats every device of the plugin as unavailable and refuses to send it commands.

### `devices`

```json
{
  "devices": [
    {"id": "1000aabbcc", "found": true, "manufacturer": "SONOFF", "model": "MINIR4", "capabilities": ["on"]},
    {"id": "1000ccbbaa", "found": true, "manufacturer": "Nanoleaf", "model": "NL45", "capabilities": ["on", "brightness"], "deviceType": "DimmableLight"},
    {"id": "1000ddeeff", "found": false, "error": "no LAN key for 1000ddeeff"}
  ],
  "undeclared": 1
}
```

- List every device from the devices file (see [Environment](#environment)).
- `found` is true once the plugin can reach the device. `manufacturer`, `model`, `capabilities` and `deviceType` matter only when it's true.
- `capabilities` lists canonical capability names from Oikade's registry (`internal/capability`), such as `on`, `brightness` or `temperature`, which [capabilities.md](capabilities.md) lists, and `contract/v1` names as constants. An old or misspelt name is unknown. A plugin can't define new capabilities. They're added to Oikade's registry, named by the guide in `internal/capability`'s package doc.
- `deviceType` is optional: what a device that's `on` or off is, if the plugin knows. It's one of `OnOffLight`, `DimmableLight`, `OnOffPlugInUnit`, `DimmablePlugInUnit`, `MountedOnOffControl` or `MountedDimmableLoadControl`, Matter's names, which `contract/v1` names as constants. Leave it out when the plugin can't tell, as for a relay. Oikade uses it unless you've set one with `oikade devices type`, and a device's other types follow from its capabilities ([OIK-142](https://linear.app/oikade/issue/OIK-142)).
- `error` is a problem with the device's configuration, such as bad `parameters` or a missing secret. Leave it empty otherwise.
- `undeclared` counts devices the plugin can see that Oikade doesn't have on record. Oikade reports it as `oikade_undeclared_devices`. A plugin that sends `discovered` can leave it at 0: Oikade then counts them from `discovered` and ignores `undeclared`.

Republish `devices` whenever any of this changes.

#### `discovered`

A plugin that can list the devices in its app (a hub, a cloud account, a LAN scan) adds them, so Oikade can adopt the ones you haven't declared ([OIK-123](https://linear.app/oikade/issue/OIK-123)):

```json
{
  "devices": [],
  "undeclared": 0,
  "discovered": [
    {"id": "3f2a9c71-0b1e-4c2d-9a8f-5e6d7c8b9a01", "manufacturer": "Signify", "model": "LCA001", "capabilities": ["on", "brightness"], "name": "Desk lamp", "room": "Study"}
  ]
}
```

- List every device in the app, **declared or not**, online or not. A device leaving the list means it was removed from its app. One that is unplugged stays in the list and is reported `found: false` in `devices` if it's declared.
- Leave `discovered` out if the plugin can't list its app's devices. An empty list means the app has none.
- `name` and `room` are as the user named them in the app. Leave them out if it has none.
- `capabilities` are canonical names, and `deviceType` is optional, as in `devices`. For a device in both, `devices` wins.
- **A discovered device's `id` is stable.** It stays the same across restarts of the plugin, and across restarts and restores of its hub or account, until the device is reset or re-paired. If the platform's own ID doesn't, build one from something that does, such as a MAC address, serial number or IEEE address. A Device that adopts it keeps that ID.

### `<id>/state`

Publish the device's full current values every time one changes. Oikade compares them with the previous values, so unchanged values cost nothing. A device that reports when it chooses and can't be asked, such as a battery sensor, may leave out what it has yet to report: Go plugins use `ReportState` for that. Use only capabilities announced for that id in `devices`. Booleans are JSON `true` or `false`, and numbers are JSON numbers within the capability's range.

### `<id>/availability`

Publish `online` when the device is reachable and `offline` when it isn't. Republish it after reconnecting to the broker, because a plugin restart follows a Last Will that marked every device offline.

### `<id>/set`

Oikade sends one capability per message, already validated against the registry and the device's announced capabilities. Apply it, then report the result through `<id>/state`. There's no acknowledgement in v1: success is the state changing. Don't retain anything on this topic.

## Relay-like devices

Relays don't know what they switch, and they don't have to: a relay reports `on`, whether it switches a light, an outlet or a fan, and leaves `deviceType` out. Oikade shows it as a plug until you say what it is with `oikade devices type`.

## Environment

A plugin runs with:

| Variable | Value |
|---|---|
| `OIKADE_MQTT_URL` | The broker, such as `mqtt://broker:1883`. |
| `OIKADE_TOPIC` | The topic root, `oikade/v1/<plugin>`. |
| `OIKADE_DEVICES_FILE` | Optional. A devices file, if you'd rather list a plugin's devices than have it report what it finds. |
| `OIKADE_SECRETS_DIR` | The directory of secrets, one file each. Under the host it's always set, and holds what `oikade secret set` wrote. |

The devices file is yours to write, and lists the devices this plugin serves.

```json
{
  "contract": 1,
  "devices": [
    {"id": "1000aabbcc", "name": "porch-light", "displayName": "Porch light", "room": "porch", "parameters": {"model": "MINIR4"}}
  ]
}
```

`parameters` is passed through without validation. Validating it is the plugin's job, and problems go in `devices[].error`.

The secrets directory holds one file per secret, by convention one per device ID. Never log its contents.

## How Oikade treats bad messages

Oikade never adopts what it can't validate. It drops the message, or the offending part of it, and counts it under `oikade_plugin_rejected_messages_total{reason}`, with one log line per plugin, reason and device:

| Reason | Cause |
|---|---|
| `malformed` | A payload that isn't the JSON or string described above, or a discovered device with an invalid or repeated `id`. |
| `wrong_contract` | `status` names a contract other than 1. Oikade then ignores the plugin until it publishes contract 1 again. |
| `unknown_capability` | A capability name not in Oikade's registry. |
| `unknown_device_type` | A `deviceType` that isn't one of the six. The device is kept, and its type follows from its capabilities. |
| `invalid_value` | A value of the wrong kind or out of range. |
| `undeclared_device` | `state` or `availability` for an id with no Device. |
| `unannounced_capability` | A value for a capability the device didn't announce in `devices`. |

## Versioning

The contract version appears twice: in the topic root (`oikade/v1`) and in `status.contract`. Additions that existing plugins and Oikade can both ignore, such as a new optional field or a new reserved topic, keep version 1. Anything else, such as renaming a topic or changing a payload's meaning, is a new version under `oikade/v2`. Oikade would then serve both while plugins move over.

## Conformance

The conformance suite is a fake hub: an in-process broker and Oikade's own decoder for this contract. It starts the plugin with a devices file and checks that:

- `status` goes online with contract 1, and `devices` lists every declared device
- every found device has availability, state and a value for each capability it announced, and one with a `deviceType` has `on`
- `status`, `devices`, `<id>/state` and `<id>/availability` are retained, and nothing is retained on `<id>/set`
- a `/set` for each writable capability is applied and reported back as state
- on SIGTERM the plugin exits 0 and leaves `status` offline
- after a restart, the plugin and its devices come back online, and it discovers every device it discovered before, by the same `id`
- when the plugin is killed, its Last Will marks it offline with contract 1
- Oikade rejected none of its messages at any point

A plugin repo runs it with `oikade-conformance`, which is versioned with the contract. A Go plugin adds it as a tool, so its version follows the `oikade` module the plugin already requires for the SDK, and a bump re-checks the plugin against the new contract. As a tool it brings Oikade's own dependencies into the plugin's `go.sum`, and Oikade's minimum Go version into its `go.mod`:

```sh
go get -tool github.com/harrymitchinson/oikade/cmd/oikade-conformance
go build -o bin/my-plugin .
go tool oikade-conformance --devices conformance-devices.json -- bin/my-plugin
```

A plugin in another language installs it in CI, which needs Go only there:

```sh
go install github.com/harrymitchinson/oikade/cmd/oikade-conformance@vX.Y.Z
oikade-conformance --devices conformance-devices.json -- ./bin/my-plugin --simulate
```

`--plugin NAME` checks a plugin a home imports, through the Go API rather than MQTT: the command is the home's binary, and the suite runs `<binary> plugin NAME` against a fake host, handing it the devices file's devices. The checks are the same.

```sh
go build -o bin/home ./home && go tool oikade-conformance --devices conformance-devices.json --plugin my-plugin -- bin/home
```

Everything after `--` is the plugin's start command, run without a shell in the current directory: a bare name is looked up in `PATH`, so write `./my-plugin` for one in the current directory. Build it first rather than use `go run` or a wrapper script: the suite signals and kills that process, and the plugin itself must be the one that gets it. Paths are absolute or relative to the current directory. The devices file must declare at least one device the plugin can find without real hardware or credentials, so plugins usually provide a simulated mode for this. `--secrets DIR` passes a directory as `OIKADE_SECRETS_DIR`, `--timeout` sets how long the plugin has to react at each step (default 15s), and `-v` logs what the decoder sees. It prints a line per check and exits 0 when all pass, 1 when one fails or the plugin can't start, and 2 on a usage error, such as a missing devices file.

Inside this repo, `make test-conformance` runs the same suite under `go test` against the simulated-relay fixture in [`test/fixtures/simswitch`](../test/fixtures/simswitch), both ways, and CI runs it on every PR. `CONFORMANCE_PLUGIN` and `OIKADE_CONFORMANCE_DEVICES` point it at another plugin, relative to the repository root.

Plugins written in Go can use [the SDK](../sdk/plugin), which implements this contract, and the payload types in [`contract/v1`](../contract/v1), which Oikade's own decoder uses. Plugins in other languages implement this document directly and pass the conformance suite above. Oikade maintains no SDK for them.

First-party plugins live in this repo, under [`plugins/`](../plugins). Anyone else's lives in its own repo, `oikade-plugin-<name>`, with its own `go.mod` and releases: a Go module that imports [`sdk/plugin`](../sdk/plugin), whose CI runs `oikade-conformance`. [`test/fixtures/simswitch`](../test/fixtures/simswitch) is a complete plugin to read first.
