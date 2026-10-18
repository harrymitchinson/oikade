# The oikade/v1 plugin contract

A plugin is a separate process that connects a device family Oikade doesn't speak natively (SONOFF LAN relays, Cozytouch heaters) to the MQTT bus. It speaks **canonical capabilities**, not vendor payloads, so Oikade needs no code for the family. The design is in [spec 0002](specs/0002-plugin-drivers.md).

This page is the contract. A plugin that follows it works with Oikade's runtime and HomeKit bridges unchanged.

## Topics

Every topic sits under the plugin's root, which the operator passes in `OIKADE_TOPIC`:

```
oikade/v1/<namespace>/<plugin>
```

`<namespace>` and `<plugin>` are the `Plugin` object's namespace and name. `<id>` is a device's plugin-local ID from `Device.spec.plugin.id`. It matches `^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`, so it never contains `/`.

| Topic | Direction | Retained | Payload |
|---|---|---|---|
| `status` | plugin → Oikade | yes | `{"state":"online","contract":1}`. Set the MQTT Last Will to `{"state":"offline","contract":1}`, retained. |
| `devices` | plugin → Oikade | yes | The devices the plugin serves. See below. |
| `<id>/state` | plugin → Oikade | yes | The device's **full** current values, such as `{"switch.on":true}`. |
| `<id>/availability` | plugin → Oikade | yes | `online` or `offline`, as a bare string. |
| `<id>/set` | Oikade → plugin | no | One capability per message, such as `{"switch.on":false}`. |

Retain everything the plugin publishes. Oikade's runtime and every HomeKit bridge pod rebuild their picture from retained topics when they start or reconnect, and they may receive those topics in any order.

Other topics under the root are reserved. Oikade ignores them.

### `status`

Publish `{"state":"online","contract":1}` once connected, and set the Last Will so that the broker publishes `{"state":"offline","contract":1}` if the plugin dies. While the status is offline, Oikade treats every device of the plugin as unavailable and refuses to send it commands.

### `devices`

```json
{
  "devices": [
    {"id": "1000aabbcc", "found": true, "manufacturer": "SONOFF", "model": "MINIR4", "capabilities": ["light.on"]},
    {"id": "1000ddeeff", "found": false, "error": "no LAN key for 1000ddeeff"}
  ],
  "undeclared": 1
}
```

- List every device from the devices file (see [Environment](#environment)).
- `found` is true once the plugin can reach the device. `manufacturer`, `model` and `capabilities` matter only when it's true.
- `capabilities` lists canonical capability names from Oikade's registry (`internal/capability`), such as `light.on`, `outlet.on`, `switch.on`, `light.level` or `sensor.temperature`. A plugin can't define new capabilities. Those go through Oikade's `add-capability` workflow.
- `error` is a problem with the device's configuration, such as bad `parameters` or a missing key in the Secret. Oikade shows it on the Device as `Resolved=False` with reason `PluginError`. Leave it empty otherwise.
- `undeclared` counts devices the plugin can see that have no Device object. Oikade reports it as `oikade_undeclared_devices`. It doesn't adopt them.

Republish `devices` whenever any of this changes.

### `<id>/state`

Publish the device's full current values every time one changes. Oikade compares them with the previous values, so unchanged values cost nothing. Use only capabilities announced for that id in `devices`. Booleans are JSON `true` or `false`, and numbers are JSON numbers within the capability's range.

### `<id>/availability`

Publish `online` when the device is reachable and `offline` when it isn't. Republish it after reconnecting to the broker, because a plugin restart follows a Last Will that marked every device offline.

### `<id>/set`

Oikade sends one capability per message, already validated against the registry and the device's announced capabilities. Apply it, then report the result through `<id>/state`. There's no acknowledgement in v1: success is the state changing. Don't retain anything on this topic.

## Relay-like devices

Relays don't know what they switch. Report `switch.on` by default. If a device's `parameters.capability` is `light.on` or `outlet.on`, report that instead, so HomeKit shows a light or an outlet. Reject any other value by setting `error` in `devices`.

## Environment

The operator runs the plugin with:

| Variable | Value |
|---|---|
| `OIKADE_MQTT_URL` | The broker, such as `mqtt://oikade-mosquitto.oikade-system.svc:1883`. |
| `OIKADE_TOPIC` | The topic root, `oikade/v1/<namespace>/<plugin>`. |
| `OIKADE_DEVICES_FILE` | `/etc/oikade/config/devices.json` |
| `OIKADE_SECRETS_DIR` | `/etc/oikade/secrets`. Present only when the Plugin sets `spec.secretRef`. |

The devices file lists the Devices declared for this plugin. The operator rewrites it and restarts the pod whenever they change.

```json
{
  "contract": 1,
  "devices": [
    {"id": "1000aabbcc", "name": "porch-light", "displayName": "Porch light", "room": "porch", "parameters": {"model": "MINIR4", "capability": "light.on"}}
  ]
}
```

`parameters` is passed through from `Device.spec.plugin.parameters` without validation. Validating it is the plugin's job, and problems go in `devices[].error`.

The secrets directory holds one file per key of the user's Secret, by convention one per device ID. The operator never reads it. Never log its contents.

## How Oikade treats bad messages

Oikade never adopts what it can't validate. It drops the message, or the offending part of it, and counts it under `oikade_plugin_rejected_messages_total{reason}`, with one log line per plugin, reason and device:

| Reason | Cause |
|---|---|
| `malformed` | A payload that isn't the JSON or string described above. |
| `wrong_contract` | `status` names a contract other than 1. Oikade then ignores the plugin until it publishes contract 1 again. |
| `unknown_capability` | A capability name not in Oikade's registry. |
| `invalid_value` | A value of the wrong kind or out of range. |
| `undeclared_device` | `state` or `availability` for an id with no Device. |
| `unannounced_capability` | A value for a capability the device didn't announce in `devices`. |

## Versioning

The contract version appears twice: in the topic root (`oikade/v1`) and in `status.contract`. Additions that existing plugins and Oikade can both ignore, such as a new optional field or a new reserved topic, keep version 1. Anything else, such as renaming a topic or changing a payload's meaning, is a new version under `oikade/v2`. Oikade would then serve both while plugins move over.

## Conformance

`test/conformance` is a fake hub: an in-process broker and Oikade's own decoder for this contract. It starts the plugin with a devices file and checks that:

- `status` goes online with contract 1, and `devices` lists every declared device
- every found device has availability, state and a value for each capability it announced
- `status`, `devices`, `<id>/state` and `<id>/availability` are retained, and nothing is retained on `<id>/set`
- a `/set` for each writable capability is applied and reported back as state
- on SIGTERM the plugin exits 0 and leaves `status` offline
- after a restart, the plugin and its devices come back online
- when the plugin is killed, its Last Will marks it offline with contract 1
- Oikade rejected none of its messages at any point

Oikade's CI runs it against [`plugins/example-switch`](../plugins/example-switch). To run it against a plugin built elsewhere, check out Oikade and pass the plugin's start command and a devices file for it. Paths are relative to the Oikade checkout unless absolute:

```sh
make test-conformance \
  CONFORMANCE_PLUGIN="node ../my-plugin/dist/main.js" \
  OIKADE_CONFORMANCE_DEVICES=../my-plugin/conformance-devices.json
```

The command is split on spaces, without a shell. The devices file must declare at least one device the plugin can find without real hardware or credentials, so plugins usually provide a simulated mode for this. `OIKADE_CONFORMANCE_SECRETS` names a directory to pass as `OIKADE_SECRETS_DIR`.

Plugins written in TypeScript can use [the SDK](../sdk/typescript), which implements this contract.
