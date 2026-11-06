---
name: add-capability
description: Add a new device capability to Oikade end to end, from the capability registry through the zigbee2mqtt binding, Matter mapping, metrics and Grafana to the docs and tests. Use this whenever the user wants Oikade to support a new kind of device state or control, such as colour temperature, illuminance, water leak, smoke, CO2, power or energy readings, window covering position, a lock or a thermostat setpoint. Use it too when a paired Zigbee device shows capabilities missing, or a zigbee2mqtt expose is "not supported yet", even if the user never says the word "capability".
---

# Adding a capability to Oikade

A capability is Oikade's protocol-neutral name for one piece of device state, such as `light.on` or `sensor.temperature`. Drivers translate their protocol to capabilities. The Matter bridge, metrics and rules only ever see capabilities. So a new capability is one small, careful change that ripples through about six places, and it is easy to finish five of them and forget the sixth. This skill is the checklist and the reasoning behind each step.

The capability's `name` and value semantics become a **public API**. They appear in Rule YAML in users' Git repos, in PromQL in their dashboards, and in `Device.status`. Renaming a capability later breaks all of that. So spend the most thought on step 1.

## 1. Define it: name, kind, units and polarity

Decide these four things before touching code, and state them to the user in one or two lines so they can object:

- **Name.** Use `<domain>.<quantity>` in kebab case, named after the physical quantity rather than the protocol. Examples: `sensor.illuminance`, `light.color-temperature`, `sensor.water-leak-detected`. Look at the existing names in `internal/capability/capability.go` and match their style. The name must make sense for drivers other than zigbee2mqtt (plugins, ESPHome later).
- **Kind.** Only `Bool` and `Number` exist (`CapabilityKind` in `api/home/v1alpha1/device_types.go`). An enumerated state such as a thermostat mode or `lock.state` doesn't fit either one. **Stop and discuss that with the user** rather than encoding the enum as magic numbers, because it needs an API change: a new kind, `make manifests generate`, `Validate`, the metrics `Float` conversion and CEL typing. Often a bool works better anyway (`lock.locked`).
- **Units and range.** Use SI units or percent, and pick the unit a person would write in a rule. Celsius, lux and watts are good. A scale that leaks the protocol, like 0–254 brightness, is not. The exception is when both sides already share a unit and the "human" unit would need a lossy conversion. Colour temperature is the example: zigbee2mqtt and Matter both use whole-number mireds, and kelvin can't make the round trip through them (2700 K comes back as 2703 K). Then prefer the shared unit, or put the trade-off to the user rather than deciding it silently. Set an honest `Min`/`Max` in the registry: `Validate` rejects out-of-range values rather than clamping them, so a range that's too tight silently drops real readings.
- **Polarity.** A bool is `true` when the condition in its name holds. `sensor.contact-open` is true when the contact is open, even though zigbee2mqtt's `contact` is true when it's closed. Name the bool so that `true` is the interesting or alarming state.
- **Writable.** Is it writable? Only make it writable if the driver can actually set it. The engine and the Matter bridge both use this flag to decide whether to accept writes.

## 2. The registry: `internal/capability`

- Add an exported const with a doc comment that states the unit and polarity. That comment is the definition other drivers will follow.
- Add the registry entry with `Kind`, `Writable`, `Min` and `Max`.
- Add `TestValidate` rows to `capability_test.go`: one valid value, one out of range, and one of the wrong type.

`TestRegistryStatus` covers the rest automatically.

## 3. The zigbee2mqtt binding: `plugins/zigbee2mqtt/exposes.go`

Find out what zigbee2mqtt actually publishes, from the device's page on zigbee2mqtt.io or from the user's `bridge/devices` output. Don't guess. Then work out:

- **Where the expose lives.** It can be top level (`type: numeric` or `binary` with a `name`) or a feature inside a composite like `light`, `switch`, `cover`, `lock` or `climate`. Add a case in the matching branch of `bindings`. Top-level numerics usually just need an entry in the `numeric` map.
- **Conversion.** `binding` already has three conversions: `scale` (raw max maps to 100%), `invert`, and `valueOn`/`valueOff`. If you need a different conversion, such as mireds to kelvin or a range with a non-zero minimum, add a field to `binding` and handle it in **both** `decode` and `encode`. Keep the two exact inverses, so that set followed by report doesn't drift. If you need a field the `expose` struct doesn't parse yet (`value_min`, `unit`, `values`), add it.
- **Writability.** If the capability is writable, check the expose's `access` bitmask allows setting it. The existing code assumes it does. `encode` output goes straight into the `/set` payload.
- **Endpoint-specific features** (`Endpoint != ""`) are skipped on purpose. Multi-endpoint devices aren't supported yet, so don't half-support them here.

Tests:

- Add a realistic device to `testdata/bridge-devices.json`, copied in shape from real zigbee2mqtt output. If it's declared, also add it to the `SetDevices` map in `setup()`.
- Extend `TestDiscovery`. The capability order follows the order of the exposes, so tests compare with `slices.Equal`.
- Extend `TestStateChanges` with a raw payload, and check the decoded canonical value.
- If it's writable, extend `TestSet` to check the encoded `/set` payload.

## 4. The Matter mapping: `internal/matterbridge/mapping.go`

Find the Matter cluster and device type for it in the Matter Device Library and Application Cluster specifications. Clusters live in `internal/matter/clusters/devices.go`, built like `NewOccupancy`, `NewTemperature` or `NewBattery`: a cluster ID, its attributes with their spec IDs and types, and its feature map. Convert units here, not in the driver: Matter measurements are integers in the spec's units, such as hundredths of a degree.

In `Map`, add the device type to `m.Types` and its cluster to `m.Clusters`, and set the attribute in `Update`. For a writable capability, turn the cluster's commands into `set` calls, like On/Off and Level Control. Add it to `Supported` so a Device with only this capability is put on the bridge.

**Endpoint IDs never move:** the controller allocates them, so the mapping can't break them. A Device that gains a capability changes its `Signature`, and the bridge rebuilds it on the same endpoint ID. That part is handled.

Some capabilities have no Matter representation, like `link.quality`. That's fine: leave them unmapped, and say so in the README table.

Tests: add cases to `internal/matterbridge/mapping_test.go` that map a Device with the capability, check its device types and clusters, push a value through `Update` and read the attribute, and, if it's writable, invoke a command and check the write. Commissioning and reads over the wire are covered by the in-process node test in `internal/matter/node`.

## 5. Metrics and Grafana

`oikade_capability_value` already exports every capability through `capability.Float`, so metrics need no code. Check whether the dashboard (`config/grafana/oikade-home.json`) should show it:

- Climate-style numbers go in a timeseries next to the temperature and humidity panels.
- On/off states go in the state timeline.
- Alarm bools (leak, smoke) are better as a stat panel that turns red, since status colours are reserved for status.

Copy an existing panel's structure and query style: `max by (device)`, and the `$namespace`/`$room` variables. Give it a fresh `id` and fit its `gridPos` in without overlapping other panels. Validate the JSON with `python3 -m json.tool` before you finish. If no panel fits, say so rather than forcing one.

## 6. Docs, then verify

- Add a row to the **Capabilities** table in `README.md` with the type, whether it's writable, and the Matter mapping. Note any polarity inversion or unit conversion.
- Update `config/samples` only if a sample device would really have this capability.

Verify. The macOS SDK breaks cgo, and golangci-lint needs an older toolchain, so run:

```sh
CGO_ENABLED=0 go test ./internal/capability/... ./internal/driver/... ./internal/matterbridge/... ./internal/engine/...
CGO_ENABLED=0 make test                 # the full suite, including envtest
CGO_ENABLED=0 GOTOOLCHAIN=go1.26.0 make lint   # building golangci-lint hits the cgo bug too
```

Run `make manifests generate` only if you changed anything under `api/`. Report what passed. Also say plainly what can't be verified without hardware: the real zigbee2mqtt payload shape (unless the user supplied it) and how the Home app renders the new device type.

## What to hand back

Give a short summary:

- the capability's definition (name, kind, unit, polarity, writable)
- the files changed
- the test results
- anything deliberately left out, such as an unmapped Matter side, a skipped dashboard panel, or endpoint variants

Don't commit unless asked.
