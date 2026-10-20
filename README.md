# Oikade

Home automation as reconciled, reviewable infrastructure.

You declare your home in Git: devices, rooms, rules and the HomeKit bridge. Kubernetes reconciles it. Live state never touches etcd. It travels over MQTT, and Prometheus keeps its history.

| | Owner | Examples |
|---|---|---|
| **Control plane** | Git, as CRDs | Which devices exist, their names and rooms, rules, the HomeKit bridge |
| **Data plane** | MQTT (Mosquitto) | "the light is on", "motion detected", "21.4°C", "turn the light off" |
| **History** | Prometheus and Grafana | Temperature over time, how often the door opened, rule runs |

Toggling a light from your phone is state, so it goes over the bus and Git doesn't care. Pairing a bulb that isn't declared in Git is drift, and Oikade reports it.

## Components

```
 drivers                           bus                        consumers
 ┌──────────────┐                                      ┌──────────────────────┐
 │ zigbee2mqtt  │◀──────▶  Mosquitto (MQTT)  ◀────────▶│ runtime              │ → Device status, /metrics, rules
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
| `Rule` | `home.oikade.dev` | Triggers, a CEL condition and actions (`do`: `set`, `delay`). |
| `Zigbee2MQTT` | `zigbee.oikade.dev` | A Zigbee2MQTT instance the operator configures and runs. |
| `Plugin` | `plugins.oikade.dev` | A plugin process the operator runs for a device platform Oikade doesn't speak natively. See [Plugins](#plugins). |
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
- **`manager runtime`** is the data plane. It follows every driver on the bus, writes coarse Device status (capabilities, availability, and `lastSeen` at most every 5 minutes), serves Prometheus metrics on `:8080` and runs rules. It runs as one replica, because two would run every rule twice.
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
kubectl -n home get zigbee2mqtts,devices,bridges,rules
```

1. **Pair devices.** Open the Zigbee2MQTT frontend (`kubectl -n home port-forward svc/z2m 8080`), permit joining, and pair. A paired device that isn't declared shows up in `oikade_undeclared_devices` and the runtime logs.
2. **Declare them.** Add a `Device` for each IEEE address. The operator sets each device's Zigbee2MQTT friendly name to the Device name.
3. **Pair the bridge.** In the Home app, choose **Add Accessory**, then **More options**, then the bridge, and enter the setup code from its Secret.
4. **Set your timezone.** Edit `--timezone` in `config/runtime/deployment.yaml` so rules see local time.

## Rules

```yaml
apiVersion: home.oikade.dev/v1alpha1
kind: Rule
metadata: {name: hallway-motion-light}
spec:
  mode: Restart          # new motion restarts the 5 minutes; Single ignores triggers while running
  triggers:
  - change: {device: hallway-motion, capability: sensor.occupancy-detected}
  condition: trigger.value && (now.getHours(tz) >= 18 || now.getHours(tz) < 7)
  do:
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

A `change` trigger or a `set` action can name a `room` instead of a `device`, but not both. A Device is in a room when its `roomRef` names it, so a lamp added to the room later is covered without editing the Rule.

```yaml
spec:
  triggers:
  - change: {room: hallway, capability: sensor.occupancy-detected}   # any sensor in the hallway
  do:
  - set: {room: living-room, capability: light.on, value: false}   # every light in the living room
```

- **Room triggers** fire on a change from any Device in the room at the time of the change. `trigger.device` is still the Device that changed.
- **Room actions** set every Device in the room that has reported the capability, in name order, and skip the rest. So `light.on` on a room leaves the motion sensor and the thermostat alone, and a Device that hasn't been discovered yet is skipped. Membership is read when the action runs, so a lamp added to the room during a `delay` is included in the actions after it.
- **If some Devices fail**, the others are still set and the remaining actions still run, so a dead bulb can't leave the rest of the room on. The run counts as `failed`. A room with nothing to set counts as `succeeded`, and the runtime logs `Matched no Devices in Room` with `--log-level=debug`.
- **Restart mode with a room trigger** restarts the run on a change from any sensor in the room, which suits people moving between sensors. Drivers only report real changes, so someone standing still in front of one sensor doesn't restart the run. Keep the `delay` longer than the time your sensor holds `occupancy: true` after the last movement.

The operator compiles every Rule and sets `Ready=False` with the reason:

- `Invalid` when the condition doesn't compile, a capability is unknown or read-only, or a value has the wrong type.
- `RoomNotFound` when a trigger or action names a Room that doesn't exist in the namespace. The message lists the missing Rooms. Creating them sets `Ready=True` without editing the Rule. The runtime doesn't check Rooms: until the Room exists it still runs the Rule, matching any Devices whose `roomRef` names it. For a misspelled room that's none. After a Room is deleted, Devices still pointing at it keep matching.

`Invalid` is reported before `RoomNotFound`. `status.lastTriggered` is updated at most once a minute.

### Migrating from Automation

`Rule` replaces the `Automation` kind, with the same behaviour ([0060](docs/design/0060-rule-model.md)). To move an Automation over:

- Change `kind: Automation` to `kind: Rule`.
- Wrap each entry of `triggers` in `change:`. For example, `- {device: m, capability: c}` becomes `- change: {device: m, capability: c}`.
- Rename `actions` to `do`. `condition`, `mode`, `delay` and room targets are unchanged.
- In PromQL, `oikade_automation_runs_total{automation=…}` is now `oikade_rule_runs_total{rule=…}`, and `oikade_automation_events_dropped_total` is now `oikade_rule_events_dropped_total`.

Apply the Rules, then delete the old Automation CRD with `kubectl delete crd automations.home.oikade.dev`. That also deletes every Automation, which the runtime stops running.

## Capabilities

Drivers translate to and from a protocol-neutral set of capabilities. HomeKit, metrics and rules only ever see these.

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

A plugin connects a device platform that isn't Zigbee, such as the eWeLink LAN protocol or the Overkiz cloud API, without changing Oikade. It targets the platform, not one device: a new device on that platform is a model in the plugin, not a new plugin. It's a separate process that speaks Oikade's capabilities over MQTT, following the [oikade/v1 plugin contract](docs/plugin-contract.md). Devices it serves are ordinary `Device`s: they show up in HomeKit, Grafana and rules like Zigbee ones, and keep working in HomeKit while the runtime is down.

```yaml
apiVersion: plugins.oikade.dev/v1alpha1
kind: Plugin
metadata: {name: ewelink-lan}
spec:
  image: ghcr.io/harrymitchinson/oikade-plugin-ewelink-lan:v0.1.0   # illustrative: plugins/example-switch is the only plugin so far
  hostNetwork: true              # for plugins that need mDNS or multicast
  secretRef: {name: ewelink-lan-keys} # mounted at /etc/oikade/secrets, one key per device id
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata: {name: porch-light}
spec:
  roomRef: {name: porch}
  plugin:
    pluginRef: {name: ewelink-lan}
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

To write a plugin in Go, use the [SDK](sdk/plugin) and the contract types in [`contract/v1`](contract/v1). A plugin's whole `main` is one `plugin.Run` call, and the SDK handles the Last Will, re-announcing after a reconnect, `/set` and SIGTERM. [`plugins/example-switch`](plugins/example-switch) is a complete example with simulated relays. `make docker-buildx-plugin PLUGIN=<dir>` builds a plugin image for arm64 and amd64, and `make test-conformance` checks a plugin against the contract. A plugin in another language implements the [contract](docs/plugin-contract.md) itself and can run the same conformance suite from an Oikade checkout.

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
| `oikade_rule_runs_total{namespace,rule,result}` | `succeeded`, `failed`, `cancelled`, `busy`, `condition_false`, `condition_error` |
| `oikade_rule_events_dropped_total` | Capability changes the rule engine dropped because it fell behind |

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
make test-e2e      # Kind: fakes Zigbee2MQTT and a plugin over MQTT and checks status, rules, metrics and the HAP pod

make test-conformance   # builds plugins/example-switch and drives it through the plugin contract
```

The plugin SDK and plugins are Go, in the same module, so `make test` and `make lint` cover them.

## Not yet

Planned work is tracked as [GitHub issues](https://github.com/harrymitchinson/oikade/issues) on the [Oikade board](https://github.com/users/harrymitchinson/projects/5). Releases are [milestones](https://github.com/harrymitchinson/oikade/milestones), and each one's description gives its goal, exit criteria and why it sits where it does. Designs live in [docs/design](docs/design/).

## Not doing

Oikade isn't a general-purpose hub. It has no Home Assistant driver and no UI for configuration.

- **Home Assistant as a driver.** Standalone device libraries that Home Assistant also uses are fine inside plugins.
- **Runtime replicas or leader election.** A single replica is deliberate, because two would run every automation twice.
- **Homebridge as a driver.** It would make Homebridge, rather than Git, the source of device identity ([0002](docs/design/0002-plugin-drivers.md)).
- **Go `plugin` or WASM plugins.** The first needs CGO, and neither can do LAN multicast usefully.
- **Configuration from a UI.** Changes made in the zigbee2mqtt frontend count as drift, by design.

## License

Copyright 2026. Licensed under the Apache License, Version 2.0.
