# The oikade/v1 plugin contract

What a plugin must say and do so Oikade can use the devices it connects: its topics, its payloads, its setup and how it's checked. It's for anyone writing a plugin, in Go or another language. A plugin is a separate process that connects a device platform to Oikade, such as the eWeLink LAN protocol or the Overkiz API. It speaks **canonical capabilities**, not vendor payloads, so Oikade needs no code for the platform. The design is in OIK-61.

A plugin that follows this page works with Oikade's runtime and Matter bridge unchanged. `contract/v1` has its payloads as Go types, and its tests check this page's examples against them.

## Two ways to run a plugin

On a host, a plugin is either [a Go module your home imports](#a-plugin-your-home-imports), or [an outside process](#an-outside-process) speaking this contract to a broker of yours (OIK-143). The messages are the same either way.

### A plugin your home imports

A plugin is a Go module whose constructor returns a `plugin.Factory`. Your home imports it and registers it, and there's no image or binary to run:

```go
home.Plugin("hall-lights", lighthub.New(lighthub.Config{Address: oikade.Var("LIGHT_HUB_ADDRESS")}))
```

**Importing a plugin means trusting its code.** It runs as the host's user, with everything that user can reach.

- **It runs in a process of its own.** The host runs your binary again as `<binary> plugin <name>`, one process per plugin, each with its own heartbeat. So a plugin that panics or hangs is restarted with backoff while automations, Matter and the other plugins carry on.
- **A plugin hangs** if its process stops answering, or a `Set` runs for longer than `oikade-host --plugin-set-timeout` (30 seconds).
- **Only a new binary or new values restart a plugin that's working,** so a deploy does, and a crash of your automations doesn't.
- **The messages are this contract's,** on the host's socket rather than a broker, so the host checks them with the same decoder. A process that dies is marked offline by the host, as a Last Will would.

A `Handler` is the same either way: `Serve` reports devices, and `Set` applies a command.

- **It reports every device it can see,** with `SetDiscovered` and `SetDevices`, since it has none on record the first time ([`devices`](#devices)). Everything it finds is in the [device list](devices.md) at once.
- **A problem with the plugin itself,** such as a login its account refuses, goes in `SetError`, which `oikade status` shows.
- **Configuration,** such as a hub's address, is Go in your home, and a password is one of your values (`oikade.Var`).

A device you forget with `oikade forget` stays forgotten while its plugin keeps listing it. If it leaves the list and returns, as a re-paired device does, it's new.

While a plugin is down, its devices are offline, and writes to them fail with an error until its next process has reported them. Claims on them hold, in the host. A claim on one is written again as soon as it reports, and a write its last process took with it is written again rather than read as somebody changing the device. A home declares a plugin device's arbitration settings in an `oikade.Control` part of the home, as for any device.

### An outside process

For a platform with no good Go library, a process of its own speaks this contract to a broker of yours. Name it to the host, and it uses the root `oikade/v1/<name>`:

```sh
oikade-host --broker mqtt://oikade@mosquitto:1883 --broker-password-secret broker-password --outside-plugins lighthub,blinds
```

- **The host supervises none of it:** the process, its restarts and its broker are yours.
- **A broker that isn't up yet is tried until it is.** Until then these plugins' devices aren't known and their claims aren't written.
- **Its devices go in the registry like any others.** [`devices`](#devices) says where it gets those on record.
- **`--broker-password-secret`** names a file in the data directory's secrets directory with the broker's password, since a password in a flag lands in `ps`.

## Topics

Every topic sits under the plugin's root, which it gets in `OIKADE_TOPIC`, or from the host:

```
oikade/v1/<plugin>
```

`<plugin>` is the plugin's name, and `<id>` is a device's plugin-local ID. Both match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`, so neither contains `/`.

| Topic | Direction | Retained | Payload |
|---|---|---|---|
| `status` | plugin → Oikade | yes | `{"state":"online","contract":1}`. Set the MQTT Last Will to `{"state":"offline","contract":1}`, retained |
| `devices` | plugin → Oikade | yes | The devices the plugin serves: see [`devices`](#devices) |
| `<id>/state` | plugin → Oikade | yes | The device's **full** current values, such as `{"on":true}`, leaving out only values a device has yet to report and can't be asked for |
| `<id>/availability` | plugin → Oikade | yes | `online` or `offline`, as a bare string |
| `<id>/set` | Oikade → plugin | no | One capability per message, such as `{"on":false}` |
| `_setup/in` | Oikade → plugin | no | A [setup](#setup) request. Only for a plugin with a setup |
| `_setup/out` | plugin → Oikade | no | A [setup](#setup) step |

Retain everything the plugin publishes but setup steps. Oikade rebuilds its picture from retained topics when it starts or reconnects, and it may receive them in any order.

Other topics under the root are reserved, and Oikade ignores them. A device ID can't start with `_`, so `_setup` is never a device.

### Oikade's own topics

Roots under `oikade/v1/` that start with `_` are Oikade's. Don't publish to them or depend on them.

### `status`

Publish `{"state":"online","contract":1}` once connected, and set the Last Will so that the broker publishes `{"state":"offline","contract":1}` if the plugin dies. While the status is offline, Oikade treats every device of the plugin as unavailable and refuses to send it commands.

| Field | Means |
|---|---|
| `state` | `online` or `offline` |
| `contract` | `1` |
| `error` | Optional. A problem of the plugin's own, that no single device carries, as with an account that refuses the password, which leaves no device to put an `error` on |
| `setup` | Optional. `true` for a plugin with a [setup](#setup) |

An `error` looks like `{"state":"online","contract":1,"error":"login refused"}`. `oikade status` shows it on one line, cut to 200 characters, so say what would fix it, such as the setting to check. Republish the status without it once the problem is fixed. It doesn't change what an online status means: the plugin is still running, and its devices say for themselves whether they can be reached (OIK-17). A problem with one device goes in that device's `error` instead.

### `devices`

```json
{
  "devices": [
    {"id": "1000aabbcc", "found": true, "manufacturer": "SONOFF", "model": "MINIR4", "firmwareVersion": "3.6.0", "capabilities": ["on"]},
    {"id": "1000ccbbaa", "found": true, "manufacturer": "Nanoleaf", "model": "NL45", "capabilities": ["on", "brightness"], "deviceType": "DimmableLight"},
    {"id": "1000ddeeff", "found": false, "error": "no LAN key for 1000ddeeff"}
  ],
  "undeclared": 1
}
```

A plugin's devices on record come from:

- **The host, for a plugin your home imports.** They're the registry's devices of that plugin, but those you forgot, which the host hands it as its session opens, and none the first time.
- **The devices file, for an outside plugin,** the one `OIKADE_DEVICES_FILE` names ([Environment](#environment)). Without one, it has none.

List every device on record, each once, by its `id`. A device listed as found that isn't on record goes on record, as does each in [`discovered`](#discovered), so a plugin with none on record reports what it can see. Oikade drops an entry whose `id` isn't valid (1 to 63 letters, digits, `_`, `.` or `-`, starting with a letter or digit) or repeats an earlier one's, and keeps the rest (OIK-317). Republish `devices` whenever any of it changes.

| A device's field | Means |
|---|---|
| `id` | Its plugin-local ID |
| `found` | True once the plugin can reach the device. The fields below but `error` matter only when it's true |
| `manufacturer`, `model`, `serialNumber`, `firmwareVersion`, `hardwareVersion` | Optional: what the device is, as its app or hub says, all strings. Oikade keeps the last of each a device had, which `oikade list devices --json` and the Home app show, so leave one out when the plugin doesn't know it rather than sending a placeholder. Matter takes at most 32 bytes of the manufacturer, model and serial number, and 64 of each version (OIK-349) |
| `capabilities` | Canonical capability names from Oikade's registry (`internal/capability`), such as `on`, `brightness` or `temperature`, which [capabilities.md](capabilities.md) lists and `contract/v1` names as constants. An old or misspelt name is unknown. A plugin can't define new capabilities: they're added to Oikade's registry, named by the guide in `internal/capability`'s package doc |
| `deviceType` | Optional: what a device that's `on` or off is, if the plugin knows. One of `OnOffLight`, `DimmableLight`, `OnOffPlugInUnit`, `DimmablePlugInUnit`, `MountedOnOffControl` or `MountedDimmableLoadControl`, Matter's names, which `contract/v1` names as constants. Leave it out when the plugin can't tell, as for a relay. Oikade uses it unless your home declares one with `Is`, and a device's other types follow from its capabilities (OIK-142) |
| `error` | A problem with the device's configuration, such as a missing key or settings the plugin can't use, which `oikade status` shows. A device can be `found` and still have one, if the plugin can see it and can't use it. Leave it empty otherwise |

| A report's field | Means |
|---|---|
| `devices` | The devices above |
| `undeclared` | How many devices the plugin can see that Oikade doesn't have on record, which Oikade reports as `oikade_undeclared_devices`. A device you forgot is on record. A plugin that sends `discovered` can leave it at 0: Oikade then counts them from `discovered` |
| `discovered` | Optional: every device in the plugin's app. See below |
| `needsSetup` | Optional: true while the plugin can't serve its devices until someone runs its [setup](#setup), such as one with no credentials yet. Only a plugin whose status has `setup` sends it |

#### `discovered`

A plugin that can list the devices in its app, such as a hub or a cloud account, adds them, so Oikade can adopt the ones you haven't declared (OIK-123):

```json
{
  "devices": [],
  "undeclared": 0,
  "discovered": [
    {"id": "3f2a9c71-0b1e-4c2d-9a8f-5e6d7c8b9a01", "manufacturer": "Signify", "model": "LCA001", "capabilities": ["on", "brightness"], "name": "Desk lamp", "room": "Study"}
  ]
}
```

- **List every device in the app, declared or not,** online or not. A device leaving the list means it was removed from its app. One that's unplugged stays in the list, and is reported `found: false` in `devices` if it's declared.
- **Leave `discovered` out if the plugin can't list its app's devices.** An empty list means the app has none. A plugin that only hears devices on the network is one of these, because it can't list one that's unplugged. It reports what it hears in `devices` with `found: true`, which puts them on record, and those on record it hasn't heard with `found: false`. So that a device you forgot stays forgotten, its first report waits until it has listened long enough to have heard the devices that are there.
- **`name` and `room`** are as the user named them in the app. Leave them out if it has none.
- **`capabilities` are canonical names, and `deviceType` is optional,** as in `devices`. For a device in both, `devices` wins, and so does each of `manufacturer`, `model`, `serialNumber`, `firmwareVersion` and `hardwareVersion` that `devices` gives.
- **A discovered device's `id` is stable.** It stays the same across restarts of the plugin, and across restarts and restores of its hub or account, until the device is reset or re-paired. If the platform's own ID doesn't, build one from something that does, such as a MAC address, serial number or IEEE address. A device that adopts it keeps that ID.

### `<id>/state`

Publish the device's full current values every time one changes. Oikade compares them with the previous values, so unchanged values cost nothing.

- **Leave out only what a device hasn't reported.** A device that reports when it chooses and can't be asked, such as a battery sensor, may leave out what it has yet to report. Go plugins use `ReportState` for that.
- **Use only capabilities announced for that `id`** in `devices`.
- **Booleans are JSON `true` or `false`,** and numbers are JSON numbers within the capability's range.
- **`mode` is the one capability whose value is a string:** one of `auto`, `heat`, `cool`, `dry` or `fan`, in lower case ([capabilities](capabilities.md)). Any other is an `invalid_value`. There's no `off` mode: a unit that's off reports `on` false, and keeps reporting the mode it will start in.

### `<id>/availability`

Publish `online` when the device is reachable and `offline` when it isn't. Republish it after reconnecting to the broker, because a plugin restart follows a Last Will that marked every device offline.

### `<id>/set`

Oikade sends one capability per message, such as `{"mode":"dry"}`, already validated against the registry and the device's announced capabilities. Apply it, then report the result through `<id>/state`. Don't retain anything on this topic.

- **A `Set` returns once the device has accepted the command,** not once it's done. A blind's `position` reports as it travels, and Oikade takes what it reports for 2 minutes after the write as the device catching up, not a person changing it.
- **A Matter controller's write fails after 5 seconds.**
- **There's no acknowledgement in v1:** success is the state changing.

## Setup

A plugin may have a setup: a short dialogue, run on the host, with the person setting it up (OIK-276). It asks only for what the plugin can't find out itself, keeps what it learns as host secrets, and checks the result. It runs where the plugin does, so its checks see the broker and the network as the plugin will. Run again, it repeats its checks and changes only what's wrong.

Oikade starts a session on `_setup/in` with `{"session":"s1","start":true}`. A plugin runs one session at a time, so a new one ends the last. The plugin then publishes its steps on `_setup/out`, numbered from 1 by `seq`, each with exactly one of these:

| Step | Example | Reply |
|---|---|---|
| `say` | `{"session":"s1","seq":1,"say":"Make an app on eWeLink's developer platform first"}` | None |
| `ask` | `{"session":"s1","seq":2,"ask":{"prompt":"App secret","secret":true}}` | What the person typed. `secret` hides it as they type |
| `open` | `{"session":"s1","seq":3,"open":{"url":"https://…/authorize?…","redirect":"http://127.0.0.1:8765/callback"}}` | The query the page redirects to `redirect` with, such as `code=abc&state=xyz` |
| `keep` | `{"session":"s1","seq":4,"keep":{"name":"1000aabbcc","value":"…"}}` | Empty, once the host has the secret on disk |
| `done` | `{"session":"s1","seq":5,"done":{"result":"ewelink-lan is set up: 2 devices online"}}` | None: the session is over |

Oikade replies on `_setup/in` with `{"session":"s1","reply":2,"value":"…"}`, naming the step it answers, or with an `error` instead of a `value` if the step failed, such as a secret the host couldn't keep. It ends a session early with `{"session":"s1","end":true}`, when the person goes, and the plugin publishes nothing more for it.

- **`done`** has a `result` saying how it went, or an `error` saying what's still wrong and what to do about it. Either is shown as it is, on one line.
- **An `open`'s `url` is https.** Its `redirect` is `http://127.0.0.1:PORT/PATH` or `http://localhost:PORT/PATH`: Oikade listens there on the person's machine, so it's also the redirect URL to register with the platform.
- **A `keep`'s `name` is a secret's name:** 1 to 253 letters, digits, `.`, `_` or `-`, starting with a letter or a digit. Every plugin on the host shares one secrets directory, so give it a name only this plugin would, by convention a device's ID. The secret appears in the secrets directory once the reply comes, and stays there for the plugin, whatever is deployed.
- **Nothing shows a secret to the person.** What they'd type in themselves, such as a password, is the home's to pass in the plugin's config. Never log a `keep`'s value, or what a `secret` ask was answered with.
- **A step Oikade can't read ends the session,** with an `end` the plugin gets, and is counted as `malformed`.
- **Setup steps carry secrets:** a `keep`'s, and a `secret` ask's answer. A plugin your home imports speaks them on the host's socket, which nothing else reads. An outside process speaks them on your broker, so anyone who can read or write its root can read them, or start a setup of their own, as they could already send it a `/set`. Give the plugin's root an ACL on a broker you share.
- **A plugin without a setup that gets a `start`** replies `{"session":"s1","seq":1,"done":{"error":"this plugin has no setup"}}`.

Go plugins implement `plugin.Setup` on their `Handler`, and the SDK speaks this dialogue for them, the reply for a plugin without one included.

## Relay-like devices

Relays don't know what they switch, and they don't have to: a relay reports `on`, whether it switches a light, an outlet or a fan, and leaves `deviceType` out. Oikade shows it as a plug until your home says what it is with `Is`.

## Environment

An outside plugin runs with these, which you set when you start it, and so does one under the conformance suite. A plugin your home imports runs with the environment the host gives every process it starts from your binary ([host-protocol.md](host-protocol.md#sessions)), which has `OIKADE_SECRETS_DIR` of these, and gets its topic root from the host as its session opens.

| Variable | Value |
|---|---|
| `OIKADE_MQTT_URL` | The broker, such as `mqtt://broker:1883`: the one the host's `--broker` names |
| `OIKADE_TOPIC` | The topic root, `oikade/v1/<plugin>` |
| `OIKADE_DEVICES_FILE` | Optional. A devices file, if you'd rather list a plugin's devices than have it report what it finds |
| `OIKADE_SECRETS_DIR` | The directory of secrets, one file each. The host sets it for a plugin your home imports, and it holds what the plugin's setup kept |

The devices file is yours to write, and lists the devices this plugin serves:

```json
{
  "contract": 1,
  "devices": [
    {"id": "1000aabbcc", "name": "porch-light", "displayName": "Porch light", "room": "porch"}
  ]
}
```

A device has no settings of its own here. A setting only the plugin uses, for one device or all of them, goes in the plugin's own config (OIK-295).

The secrets directory holds one file per secret, by convention one per device ID. Never log its contents. In the Go SDK, a plugin your home imports takes a credential the person chooses, such as a password, as a `string` in its config, which the home gives with `oikade.Var` from its `.env`, and reads what its setup kept with `plugin.ReadSecret`.

## How Oikade treats bad messages

Oikade never adopts what it can't validate. It drops the message, or the offending part of it, and counts it under `oikade_plugin_rejected_messages_total{reason}`, with one log line per plugin, reason and device:

| Reason | Cause |
|---|---|
| `malformed` | A payload that isn't the JSON or string described above, a device or a discovered device with an invalid or repeated `id`, or a setup step that isn't one of the five |
| `wrong_contract` | `status` names a contract other than 1. Oikade then ignores the plugin until it publishes contract 1 again |
| `unknown_capability` | A capability name not in Oikade's registry |
| `unknown_device_type` | A `deviceType` that isn't one of the six. The device is kept, and its type follows from its capabilities |
| `invalid_value` | A value of the wrong kind or out of range |
| `undeclared_device` | `state` or `availability` for an `id` with no device. One for a device you forgot is dropped without counting, since a plugin that hears devices on the network can't tell it from a new one |
| `unannounced_capability` | A value for a capability the device didn't announce in `devices` |

## Versioning

The contract's version appears twice: in the topic root (`oikade/v1`) and in `status.contract`. Additions that existing plugins and Oikade can both ignore, such as a new optional field or a new reserved topic, keep version 1. Anything else, such as renaming a topic or changing a payload's meaning, is a new version under `oikade/v2`, which Oikade would serve beside v1 while plugins move over.

## Conformance

The conformance suite is a fake hub: an in-process broker and Oikade's own decoder for this contract. It starts the plugin with a devices file and checks that:

- `status` goes online with contract 1, and `devices` lists every declared device
- every found device with no `error` has availability, state and a value for each capability it announced, and one with a `deviceType` has `on`. One found with an `error` needs none of them, and the suite doesn't set it
- `status`, `devices`, `<id>/state` and `<id>/availability` are retained, and nothing is retained on `<id>/set`
- a `/set` for each writable capability is applied and reported back as state: the opposite bool, a number in the range, or the next of `mode`'s names
- a plugin whose status has `setup` runs its setup twice, each ending in a `done` with no `error`. The suite answers each `ask` and `open` from `--setup-replies`, then with empty strings, and says every `keep` is kept without keeping it
- on SIGTERM the plugin exits 0 and leaves `status` offline
- after a restart, the plugin and its devices come back online, and it discovers every device it discovered before, by the same `id`
- when the plugin is killed, its Last Will marks it offline with contract 1
- Oikade rejected none of its messages at any point

### Running it

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

`--plugin NAME` checks a plugin a home imports, whose messages go over the host's socket (`internal/host/pluginwire`) rather than a broker. The command is then the home's binary, and the suite runs `<binary> plugin NAME` against a fake host on a socket of its own, handing it the devices file's devices as those on record. The checks are the same:

```sh
go build -o bin/home ./home && go tool oikade-conformance --devices conformance-devices.json --plugin my-plugin -- bin/home
```

Everything after `--` is the plugin's start command, run without a shell in the current directory. A bare name is looked up in `PATH`, so write `./my-plugin` for one in the current directory. Build it first rather than use `go run` or a wrapper script: the suite signals and kills that process, and the plugin itself must be the one that gets it. The devices file must declare at least one device the plugin can find without real hardware or credentials, so plugins usually provide a simulated mode for this.

| Flag | Means |
|---|---|
| `--devices FILE` | The devices file to hand the plugin. Required |
| `--plugin NAME` | Check a plugin a home's binary imports, over a fake host's socket |
| `--secrets DIR` | A directory to pass as `OIKADE_SECRETS_DIR` |
| `--setup-replies FILE` | A JSON list of strings, in order, that answer the plugin's setup |
| `--timeout D` | How long the plugin has to react at each step, 15s unless it's given |
| `-v` | Log what the decoder sees |

Paths are absolute or relative to the current directory. It prints a line per check, and exits 0 when all pass, 1 when one fails or the plugin can't start, and 2 on a usage error, such as a missing devices file.

### In this repo

`make test-conformance` runs the same suite under `go test` against the simulated-relay fixture in [`test/fixtures/simswitch`](../test/fixtures/simswitch), both ways, and CI runs it on every PR. `make test-conformance CONFORMANCE_PLUGIN=COMMAND` points it at another plugin, with `OIKADE_CONFORMANCE_DEVICES` naming its devices file, both relative to the repository root.

### Writing one

Plugins written in Go can use [the SDK](../sdk/plugin), which implements this contract, and the payload types in [`contract/v1`](../contract/v1), which Oikade's own decoder uses. Plugins in other languages implement this page directly and pass the conformance suite above. Oikade maintains no SDK for them.

First-party plugins live in this repo, under [`plugins/`](../plugins). Anyone else's lives in its own repo, `oikade-plugin-<name>`, with its own `go.mod` and releases: a Go module that imports [`sdk/plugin`](../sdk/plugin), whose CI runs `oikade-conformance`. [`test/fixtures/simswitch`](../test/fixtures/simswitch) is a complete plugin to read first.
