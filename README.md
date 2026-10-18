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
 └──────────────┘                                  └──▶│ HAP bridge (per      │ → the Home app and Siri
        ▲                                              │   Bridge)            │
        └── operator: renders configuration.yaml from Devices ─────────────────┘
```

| Kind | Group | Purpose |
|------|-------|---------|
| `Device` | `home.oikade.dev` | A device that should exist: name, room and driver source (Zigbee IEEE address). Status shows its capabilities and availability. |
| `Room` | `home.oikade.dev` | Labels metrics and groups devices. HomeKit can't receive rooms from a bridge, so you also assign them once in the Home app. |
| `Automation` | `home.oikade.dev` | Triggers, a CEL condition and actions (`set`, `delay`). |
| `Zigbee2MQTT` | `zigbee.oikade.dev` | A Zigbee2MQTT instance the operator configures and runs. |
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

The operator compiles every Automation and sets `Ready=False` with the reason when the condition doesn't compile, a capability is unknown or read-only, or a value has the wrong type. `status.lastTriggered` is updated at most once a minute.

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

## Metrics

The runtime serves these on `:8080/metrics`. Enable `../prometheus` in `config/default` for a ServiceMonitor, and `../grafana` for the "Oikade home" dashboard.

| Metric | |
|---|---|
| `oikade_capability_value{namespace,device,room,capability}` | Current value. Bools are 0 or 1. |
| `oikade_capability_changes_total{…}` | Counts changes, including ones shorter than the scrape interval (a door opened and closed within 15s) |
| `oikade_device_available{namespace,device,room}` | 1 online, 0 offline |
| `oikade_device_last_seen_timestamp_seconds{…}` | |
| `oikade_undeclared_devices{namespace,instance}` | Paired but not in Git |
| `oikade_automation_runs_total{namespace,automation,result}` | `succeeded`, `failed`, `cancelled`, `busy`, `condition_false`, `condition_error` |

Every capability change is also logged as `"Changed capability"` with structured fields, so Loki has the event history.

## State that must survive

Losing these means re-pairing, and then the Home app forgets every room, scene and automation, or every Zigbee device has to be paired again.

- **The HomeKit identity** (`<bridge>-identity` Secret) and **pairings** (`<bridge>-hap` PVC) have no owner reference. Deleting a Bridge keeps them. If the Secret is deleted, it is restored from `Bridge.status.username`.
- **Accessory IDs** live in `Bridge.status.accessories`. They are only ever appended: never reused or reordered, and kept for deleted Devices, so a Device that comes back gets its old ID.
- **The Zigbee network key** (`<instance>-network` Secret) and **database** (`<instance>-data` PVC) have no owner reference either. Back up the Secret.
- **Zigbee2MQTT's `configuration.yaml` is overwritten on every start** with the version rendered from Git. Changes made in its frontend don't survive a restart.

To really forget a Bridge or Zigbee2MQTT instance, delete those objects yourself:

```sh
kubectl delete secret,pvc -l homekit.oikade.dev/bridge=<name>
kubectl delete secret,pvc -l zigbee.oikade.dev/instance=<name>
```

## Security notes

- Mosquitto allows anonymous access inside the cluster. Anything that can reach it can control your devices. Restrict it with a NetworkPolicy, or bring your own broker and set `--mqtt-url` on the operator, runtime and Bridge pods.
- HAP pods use the host network, run as non-root and only have read access to the API. Each one can read only its own setup code and identity Secret.

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

`1.0.0` is a deliberate decision, not something a commit triggers. The earlier date tag `2026.42.1` isn't part of this sequence; don't pin to it.

## Development

```sh
make test          # unit tests, envtest, and an in-process MQTT broker for the runtime integration test
make lint
make test-e2e      # Kind: fakes Zigbee2MQTT over MQTT and checks status, automations, metrics and the HAP pod
```

## Not yet

Planned work, its order and what's deliberately out of scope are in the [roadmap](docs/ROADMAP.md). Designs live in [docs/specs](docs/specs/).

## License

Copyright 2026. Licensed under the Apache License, Version 2.0.
