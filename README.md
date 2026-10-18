# Oikade

Home automation as reconciled, reviewable infrastructure.

You declare your home in Git: devices, rooms, automations and the HomeKit bridge. Kubernetes reconciles it. Live state never touches etcd. It travels over MQTT, and Prometheus keeps its history.

| | Owner | Examples |
|---|---|---|
| **Control plane** | Git, as CRDs | Which devices exist, their names and rooms, automations, the HomeKit bridge |
| **Data plane** | MQTT (Mosquitto) | "the light is on", "motion detected", "21.4°C", "turn the light off" |
| **History** | Prometheus and Grafana | Temperature over time, how often the door opened, automation runs |

Toggling a light from your phone is state, so it goes over the bus and Git doesn't care. Pairing a bulb that isn't declared in Git is drift, and Oikade reports it.

## Components

```
 drivers                           bus                        consumers
 ┌──────────────┐                                      ┌──────────────────────┐
 │ zigbee2mqtt  │◀──────▶  Mosquitto (MQTT)  ◀────────▶│ runtime              │ → Device status, /metrics, automations
 ├──────────────┤             ▲                    └──▶│ HAP bridge (per      │ → the Home app and Siri
 │ plugins      │◀────────────┘                        │   Bridge)            │
 └──────────────┘                                      └──────────────────────┘
        ▲
        └── operator: renders configuration.yaml and devices.json from Devices
```

| Kind | Group | Purpose |
|------|-------|---------|
| `Device` | `home.oikade.dev` | A device that should exist: name, room and driver source (a Zigbee IEEE address, or a Plugin and a plugin-local id). Status shows its capabilities and availability. |
| `Room` | `home.oikade.dev` | Labels metrics and groups devices. HomeKit can't receive rooms from a bridge, so you also assign them once in the Home app. |
| `Automation` | `home.oikade.dev` | Triggers, a CEL condition and actions (`set`, `delay`). |
| `Zigbee2MQTT` | `zigbee.oikade.dev` | A Zigbee2MQTT instance the operator configures and runs. |
| `Plugin` | `plugins.oikade.dev` | A plugin process the operator runs for a device family Oikade doesn't speak natively. See [Plugins](#plugins). |
| `Bridge` | `homekit.oikade.dev` | A HomeKit bridge that exposes a set of Devices. |

Illustrative output:

```console
$ kubectl -n home get devices
NAME             NAME             ROOM      CAPABILITIES                                  AVAILABLE   LAST SEEN
hallway-light    Hallway Light    hallway   ["light.on","light.level","link.quality"]     True        2m
hallway-motion   Hallway Motion   hallway   ["sensor.occupancy-detected",...]             True        1m
```

There is one binary with three subcommands:

- **`manager`** is the operator. It only talks to Kubernetes.
- **`manager runtime`** is the data plane. It follows every driver on the bus, writes coarse Device status (capabilities, availability, and `lastSeen` at most every 5 minutes), serves Prometheus metrics on `:8080` and runs automations. It runs as one replica, because two would run every automation twice.
- **`manager hap --bridge <name>`** is a HomeKit accessory server, started by the operator for each `Bridge`. It follows the bus itself, so HomeKit keeps working if the runtime is down. It never writes to the API.

## Requirements

- **The HAP pod's node must be on the same L2 network as your Apple devices.** HomeKit finds bridges over mDNS, so the HAP pod uses `hostNetwork: true`. Kind or minikube on macOS run inside a VM, where the Home app can't see them. Use those for development, and a real Linux node (k3s, Talos, etc.) for pairing.
- **A Zigbee coordinator.** A network coordinator (`tcp://host:6638`, e.g. SLZB-06) lets the Zigbee2MQTT pod run anywhere. A USB stick (`/dev/ttyUSB0`) needs `nodeSelector` to pin the pod to that node, and the pod then runs privileged.
- A default StorageClass.

## Quick start

For a full walkthrough on real hardware, from blank Pis to a paired bridge, with GitOps, monitoring and backups, see [Deploying on a Raspberry Pi cluster](docs/deploy-raspberry-pi.md).

```sh
make docker-build docker-push IMG=<registry>/oikade:dev
make deploy IMG=<registry>/oikade:dev      # operator, runtime and Mosquitto in oikade-system

kubectl create namespace home
# Edit config/samples first: the coordinator address, IEEE addresses and the setup code.
kubectl -n home apply -k config/samples
kubectl -n home get zigbee2mqtts,devices,bridges,automations
```

1. **Pair devices.** Open the Zigbee2MQTT frontend (`kubectl -n home port-forward svc/z2m 8080`), permit joining, and pair. A paired device that isn't declared shows up in `oikade_undeclared_devices` and the runtime logs.
2. **Declare them.** Add a `Device` for each IEEE address. The operator sets each device's Zigbee2MQTT friendly name to the Device name.
3. **Pair the bridge.** In the Home app, choose **Add Accessory**, then **More options**, then the bridge, and enter the setup code from its Secret.
4. **Set your timezone.** Edit `--timezone` in `config/runtime/deployment.yaml` so automations see local time.

## Automations

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Automation
metadata: {name: hallway-motion-light}
spec:
  mode: Restart          # new motion restarts the 5 minutes; Single ignores triggers while running
  triggers:
  - {device: hallway-motion, capability: sensor.occupancy-detected}
  condition: trigger.value && (now.getHours(tz) >= 18 || now.getHours(tz) < 7)
  actions:
  - set: {device: hallway-light, capability: light.on, value: true}
  - delay: 5m
  - set: {device: hallway-light, capability: light.on, value: false}
```

The condition is [CEL](https://cel.dev). It can use these variables:

| Variable | Type | |
|---|---|---|
| `trigger` | map | `device`, `capability`, `value`, `previous` (null the first time) |
| `devices` | map | Device name → capability → current value, for every Device in the namespace |
| `now` | timestamp | |
| `tz` | string | The runtime's `--timezone`. CEL's accessors default to UTC, so write `now.getHours(tz)`. |

### Rooms

A trigger or a `set` action can name a `room` instead of a `device`, but not both. A Device is in a room when its `roomRef` names it, so a lamp added to the room later is covered without editing the Automation.

```yaml
spec:
  triggers:
  - {room: hallway, capability: sensor.occupancy-detected}   # any sensor in the hallway
  actions:
  - set: {room: living-room, capability: light.on, value: false}   # every light in the living room
```

- **Room triggers** fire on a change from any Device in the room at the time of the change. `trigger.device` is still the Device that changed.
- **Room actions** set every Device in the room that has reported the capability, in name order, and skip the rest. So `light.on` on a room leaves the motion sensor and the thermostat alone, and a Device that hasn't been discovered yet is skipped. Membership is read when the action runs, so a lamp added to the room during a `delay` is included in the actions after it.
- **If some Devices fail**, the others are still set and the remaining actions still run, so a dead bulb can't leave the rest of the room on. The run counts as `failed`. A room with nothing to set counts as `succeeded`, and the runtime logs `Matched no Devices in Room` with `--log-level=debug`.
- **Restart mode with a room trigger** restarts the run on a change from any sensor in the room, which suits people moving between sensors. Drivers only report real changes, so someone standing still in front of one sensor doesn't restart the run. Keep the `delay` longer than the time your sensor holds `occupancy: true` after the last movement.

The operator compiles every Automation and sets `Ready=False` with the reason:

- `Invalid` when the condition doesn't compile, a capability is unknown or read-only, or a value has the wrong type.
- `RoomNotFound` when a trigger or action names a Room that doesn't exist in the namespace. The message lists the missing Rooms. Creating them sets `Ready=True` without editing the Automation. The runtime doesn't check Rooms: until the Room exists it still runs the Automation, matching any Devices whose `roomRef` names it. For a misspelled room that's none. After a Room is deleted, Devices still pointing at it keep matching.

`Invalid` is reported before `RoomNotFound`. `status.lastTriggered` is updated at most once a minute.

## Capabilities

Drivers translate to and from a protocol-neutral set of capabilities. HomeKit, metrics and automations only ever see these.

| Capability | Type | Writable | HomeKit |
|---|---|---|---|
| `light.on` | bool | ✓ | Lightbulb |
| `light.level` | 0–100 | ✓ | Lightbulb brightness. Independent of `light.on`. |
| `switch.on` | bool | ✓ | Switch, or Outlet/Lightbulb with `spec.homekit.service` |
| `outlet.on` | bool | ✓ | Outlet |
| `sensor.temperature` | °C | | Temperature sensor |
| `sensor.relative-humidity` | % | | Humidity sensor |
| `sensor.contact-open` | bool | | Contact sensor. `true` means open, the inverse of Zigbee2MQTT's `contact`. |
| `sensor.occupancy-detected` | bool | | Motion sensor, or Occupancy sensor with `spec.homekit.service` |
| `battery.level` | % | | Battery, with a low-battery warning below 20% |
| `link.quality` | 0–255 | | (metrics only) |

## Plugins

A plugin connects a device family that isn't Zigbee, such as SONOFF LAN relays or Cozytouch heaters, without changing Oikade. It's a separate process that speaks Oikade's capabilities over MQTT, following the [oikade/v1 plugin contract](docs/plugin-contract.md). Devices it serves are ordinary `Device`s: they show up in HomeKit, Grafana and automations like Zigbee ones, and keep working in HomeKit while the runtime is down.

```yaml
apiVersion: plugins.oikade.dev/v1alpha1
kind: Plugin
metadata: {name: sonoff}
spec:
  image: ghcr.io/harrymitchinson/oikade-plugin-sonoff:v0.1.0   # illustrative: plugins/example-switch is the only plugin so far
  hostNetwork: true              # for plugins that need mDNS or multicast
  secretRef: {name: sonoff-keys} # mounted at /etc/oikade/secrets, one key per device id
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: porch-light}
spec:
  roomRef: {name: porch}
  plugin:
    pluginRef: {name: sonoff}
    id: 1000aabbcc               # plugin-local and stable
    parameters: {capability: light.on}   # passed through; the plugin validates them
```

The operator runs each Plugin as a one-replica Deployment `<plugin>-plugin`, and writes the Devices declared for it to `devices.json` in the Secret `<plugin>-plugin-config`. Any change to the Devices it serves restarts the pod: adding or removing one, or changing its id, name, display name, room or parameters. So does a change to the `secretRef` Secret. Plugins are trusted images you build: they get no service account token and run as non-root with a read-only root filesystem.

The Plugin's `Ready` condition is `True` with reason `Available` once the pod runs and nothing below applies. Otherwise it's `False` with one of these reasons, and `DuplicateDevice`, `SecretNotFound` and `NameConflict` show even while the pod is running:

- `Progressing`: the pod isn't available yet
- `SecretNotFound`: the `secretRef` Secret doesn't exist. The plugin still runs and reports an error for each device that needs a key.
- `DuplicateDevice`: two Devices claim the same `id`. The first by name wins.
- `NameConflict`: `<plugin>-plugin` or `<plugin>-plugin-config` exists and belongs to something else, or `secretRef` names `<plugin>-plugin-config`

A plugin reports problems with a device, such as bad parameters or a missing key, as `Resolved=False` with reason `PluginError` on the Device. Messages Oikade can't validate are dropped and counted in `oikade_plugin_rejected_messages_total`.

To write a plugin in TypeScript, use the [SDK](sdk/typescript). [`plugins/example-switch`](plugins/example-switch) is a complete example with simulated relays. `make docker-buildx-plugin PLUGIN=<dir>` builds a plugin image for arm64 and amd64, and `make test-conformance` checks a plugin against the contract.

## Metrics

The runtime serves these on `:8080/metrics`. Enable `../prometheus` in `config/default` for a ServiceMonitor, and `../grafana` for the "Oikade home" dashboard.

| Metric | |
|---|---|
| `oikade_capability_value{namespace,device,room,capability}` | Current value. Bools are 0 or 1. |
| `oikade_capability_changes_total{…}` | Counts changes, including ones shorter than the scrape interval (a door opened and closed within 15s) |
| `oikade_device_available{namespace,device,room}` | 1 online, 0 offline |
| `oikade_device_last_seen_timestamp_seconds{…}` | |
| `oikade_undeclared_devices{namespace,instance}` | Paired but not in Git |
| `oikade_plugin_up{namespace,plugin}` | 1 while the plugin reports itself online with contract 1 |
| `oikade_plugin_rejected_messages_total{namespace,plugin,reason}` | Plugin messages dropped as `malformed`, `wrong_contract`, `unknown_capability`, `invalid_value`, `undeclared_device` or `unannounced_capability` |
| `oikade_automation_runs_total{namespace,automation,result}` | `succeeded`, `failed`, `cancelled`, `busy`, `condition_false`, `condition_error` |

Every capability change is also logged as `"Changed capability"` with structured fields, so Loki has the event history.

## State that must survive

Losing these means re-pairing, and then the Home app forgets every room, scene and automation, or every Zigbee device has to be paired again.

- **The HomeKit identity** (`<bridge>-identity` Secret) and **pairings** (`<bridge>-hap` PVC) have no owner reference. Deleting a Bridge keeps them. If the Secret is deleted, it is restored from `Bridge.status.username`.
- **Accessory IDs** live in `Bridge.status.accessories`. They are only ever appended: never reused or reordered, and kept for deleted Devices, so a Device that comes back gets its old ID.
- **The Zigbee network key** (`<instance>-network` Secret) and **database** (`<instance>-data` PVC) have no owner reference either. Back up the Secret.
- **Zigbee2MQTT's `configuration.yaml` is overwritten on every start** with the version rendered from Git. Changes made in its frontend don't survive a restart.
- **A plugin's credentials** (the `spec.secretRef` Secret, such as LAN keys) belong to you. The operator never creates, changes or deletes them, so keep them in Git, encrypted with SOPS or similar. Everything else a plugin needs is rendered from Git, and plugins keep no state of their own.

To really forget a Bridge or Zigbee2MQTT instance, delete those objects yourself:

```sh
kubectl delete secret,pvc -l homekit.oikade.dev/bridge=<name>
kubectl delete secret,pvc -l zigbee.oikade.dev/instance=<name>
```

## Security notes

- Mosquitto allows anonymous access inside the cluster. Anything that can reach it can control your devices. Restrict it with a NetworkPolicy, or bring your own broker and set `--mqtt-url` on the operator, runtime and Bridge pods.
- HAP pods use the host network, run as non-root and only have read access to the API. Each one can read only its own setup code and identity Secret.
- Plugins publish on the same anonymous broker, so anything on the cluster network could also publish as a plugin, just as it could as Zigbee2MQTT.

## Logging

The operator, `runtime` and `hap` commands write structured JSON logs to stderr using
Go's `log/slog`, including controller-runtime logs. Use `--log-level=debug|info|warn|error`
to set the minimum level (default: `info`). Errors are always logged.
The former `--zap-*` flags are no longer supported.

## Versioning

Releases follow [semantic versioning](https://semver.org), and Git tags and image tags use the same version, for example `v0.1.0`. Oikade's public API is the CRDs, capability names, CEL variables, metric names and labels, and HomeKit accessory IDs.

While the major version is 0, the API is still settling:

- A breaking change to the public API bumps MINOR (`v0.1.3` → `v0.2.0`). Read the release notes before upgrading.
- Features and fixes bump PATCH (`v0.1.0` → `v0.1.1`).

`1.0.0` is a deliberate decision, not something a commit triggers.

## Development

```sh
make test          # unit tests, envtest, and an in-process MQTT broker for the runtime integration test
make lint
make test-e2e      # Kind: fakes Zigbee2MQTT and a plugin over MQTT and checks status, automations, metrics and the HAP pod

make test-plugins       # the TypeScript workspace (sdk/typescript, plugins/): format, lint, typecheck, unit tests
make test-conformance   # drives plugins/example-switch through the plugin contract
```

The TypeScript workspace needs Node 22 or 24. Yarn comes from `packageManager` through corepack.

## Not yet

Planned work is tracked in [Backlog.md](backlog/). The [roadmap](backlog/docs/doc-0005%20-%20Roadmap.md) gives the order and what's deliberately out of scope, and designs live in [backlog/docs/specs](backlog/docs/specs/). Run `backlog board` or `backlog browser` to see it all.

## License

Copyright 2026. Licensed under the Apache License, Version 2.0.
