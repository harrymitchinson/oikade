# 0002: Plugin drivers

> **Review (2026-10-19).** An agent made and accepted this design without the user's review. The user has since confirmed these parts of it:
> - out-of-process plugins over MQTT (option B)
> - the `oikade/v1` topic layout, which is versioned and can be extended
> - the `Plugin` kind in `plugins.oikade.dev`
> - `Device.spec.plugin` with opaque `parameters`
> - one credentials Secret per Plugin
> - the relay convention
>
> [0006](0006-go-plugin-sdk.md) revises where the SDK lives and its language: Go first, in this repo, with no handwritten TypeScript SDK here.

## Problem

The hall and porch lights run on SONOFF MINIR4 relays, and the pool pump runs on a SONOFF TH Elite. None of them is Zigbee, so Oikade can't see them. They live in a separate Homebridge with their own plugins (`../homebridge-plugins/plugins/sonoff-minir4`, `sonoff-th-elite-pool`). As a result, they don't appear in Grafana, Oikade automations can't use them ("porch light on at sunset" can't name the porch relay), and they're the only devices in the house not declared in Git.

Every device family after this one (Cozytouch heaters are already in `../homebridge-plugins/packages/overkiz-cozytouch`, Shelly and ESPHome are next) hits the same wall. Today, adding a family means changing Oikade's core. The hub constructs `zigbee2mqtt.Instance` directly (`internal/hub/hub.go` `ensure`), `DeviceSpec` has a single driver source, and its CEL rule is `has(self.zigbee2mqtt)`.

Done looks like this:

- A device served by a plugin is a `Device` in Git. It shows up in HomeKit through an Oikade Bridge and in Grafana, and an Automation can switch it. The simulated example plugin proves this.
- Adding a device family means writing a plugin, without editing `internal/hub` or `api/home`.
- A plugin can reuse existing protocol code such as `@harrymitchinson/sonoff-lan`, rather than rewriting it.

This spec designs and builds the **plugin system**. Concrete plugins (SONOFF MINIR4, TH Elite, Cozytouch) are separate pieces of work built on top of it, and aren't part of this spec.

## Constraints

- **Plane.** A plugin instance and its devices are config (CRDs). LAN keys go in Secrets. Live relay state and temperature go on MQTT. Nothing fast-changing goes into `status`.
- **Public API.** This creates a new public surface: the wire contract between Oikade and plugins (topics and payloads). That contract is the most expensive thing here to rename, so it is versioned in the topic (`oikade/v1/...`). `DeviceSpec` gains a second source, and its CEL rule relaxes to "exactly one of". Existing YAML stays valid. Plugins must report **canonical capability names** from `internal/capability`. Capabilities, metrics, CEL variables and HomeKit IIDs are unchanged, because HAP mapping is capability-based.
- **State that must survive.** LAN keys are user-supplied Secrets, from Git via SOPS or similar, with no owner reference. They follow the same pattern as the Zigbee network key. A plugin that keeps its own state, such as the pool scheduler's saved plan, would need a PVC. That's out of scope for v1 (see Non-goals).
- **Component boundaries.**
  - `manager` reconciles a plugin CR into a Deployment and a rendered device ConfigMap, as it already does for Zigbee2MQTT. It doesn't talk to MQTT.
  - The hub (used by `runtime` *and* every `hap` pod) gains a generic decoder for the plugin contract. So HomeKit keeps working when the runtime is down.
  - Writes go out through `driver.Set` as an MQTT `/set` to the plugin.
- **Pi.** Plugin images must be arm64. Some plugins need mDNS multicast (SONOFF LAN reports, for example), so the `Plugin` kind offers `hostNetwork`, as HAP already uses. A Node plugin costs about 60–100 MiB RSS. It's one pod per plugin instance, not per device.

## Options

### A. In-tree Go drivers

Port `sonoff-lan` to Go. Add a `sonoff.oikade.dev` group with an instance kind, and a `manager gateway sonoff` role that talks LAN and publishes to MQTT. Add a matching decoder in the hub. "SDK" here means a Go driver registry inside Oikade.

- Pros: one language and one image, with typed, envtest-covered code paths. There's no new public wire contract, because each gateway/decoder pair is private to Oikade.
- Cons: every device family is a core change and a new CRD group. The carefully hardened TS protocol code (confirmation semantics, replay notes, emulator) gets rewritten. The Cozytouch cloud client would be rewritten again later.
- Cost of reversing: low for users (the CRDs are per-family), but high in effort already spent porting.

### B. Out-of-process plugins over a canonical MQTT contract

Oikade defines a small, versioned MQTT contract in which plugins speak *canonical capabilities*, not vendor payloads. A new `plugins.oikade.dev` `Plugin` kind tells the manager what to run: the image, a Secret, and `hostNetwork`. Devices reference a Plugin by name plus a plugin-local device ID. The manager renders the declared devices into a ConfigMap the plugin reads, so Git owns naming, as with zigbee2mqtt. Each plugin is a thin process: in TypeScript, a small `@harrymitchinson/oikade-plugin-sdk` wrapping a controller such as `SonoffMiniR4Controller`. A Go SDK can follow when there's a Go plugin.

```
oikade/v1/<ns>/<plugin>/status              retained, online|offline (LWT)
oikade/v1/<ns>/<plugin>/devices             retained, [{id, found, manufacturer, model, capabilities, error}]
oikade/v1/<ns>/<plugin>/<id>/state          retained, {"light.on": true}
oikade/v1/<ns>/<plugin>/<id>/availability   retained, online|offline
oikade/v1/<ns>/<plugin>/<id>/set            {"light.on": false}
```

- Pros:
  - It's language-agnostic, and reuses `sonoff-lan` and `overkiz-cozytouch` as they are.
  - A crashing plugin can't take down the runtime.
  - The hub becomes driver-agnostic with one generic decoder, used by runtime and HAP alike.
  - Capability validation stays in Oikade: an unknown capability is dropped and counted, never adopted.
  - It generalises what zigbee2mqtt already is, a separate gateway process on the bus.
- Cons:
  - The contract becomes a public API that needs versioning and conformance tests.
  - Plugin-specific device options lose CRD-level validation. The plugin validates them and reports `error` in `devices`, which the status writer surfaces as a condition.
  - It adds a Node image per family on the Pi.
- Cost of reversing: medium. Plugins are all the user's own, so a `v2` contract means updating a handful of repos. Device YAML (`spec.plugin`) would survive a change of transport.

### C. Homebridge as a driver

Run the existing Homebridge plugins unchanged in a Homebridge pod. Oikade reads them either as a HomeKit controller pairing with Homebridge's bridge, or through a Homebridge-to-MQTT plugin. Then it maps HAP services back to capabilities.

- Pros: zero changes to the two plugins. It opens up the whole Homebridge plugin catalogue.
- Cons:
  - The mapping is lossy (HomeKit service, then capability, then HomeKit again), and there are two HomeKit stacks in one home. The Homebridge bridge must never be paired with the Home app.
  - It's the heaviest option on the Pi (Node plus Homebridge plus plugins).
  - Device identity is Homebridge's accessory UUIDs, not Git.
  - The pool scheduler's logic stays hidden inside Homebridge, where Oikade can't see it.
- Cost of reversing: low (one driver), but it entrenches Homebridge as a dependency.

*Considered and dropped:*

- **Reflashing the SONOFFs with Tasmota or ESPHome** and writing an MQTT-native driver for that firmware. It's a good fit for future devices, but it needs serial flashing of installed mains devices and doesn't help with cloud-only Cozytouch.
- **Go `plugin` or WASM.** The first needs CGO and matching builds. The second can't do multicast or LAN sockets usefully.

## Decision

**Accepted: B.** The problem is "every new family is a core change", and B is the only option that removes that while reusing the protocol code that already exists. A is still a good choice for any family that's simple enough to be a plain MQTT decoder, as zigbee2mqtt is. Shelly or Tasmota could later be in-tree decoders *or* plugins. C reuses the most code today, but makes Homebridge, not Git, the source of device identity. B gives up CRD validation of plugin-specific options and accepts a new wire contract to maintain. The contract is deliberately small (five topics) to keep that cost down.

Scope: the contract, the `Plugin` kind, the generic hub decoder, the TypeScript SDK, the conformance suite and a simulated example plugin. No real device plugins.

Settled in review:

- **One Secret per Plugin**, with one key per device ID. The manager mounts it into the plugin pod by name. It checks that the Secret exists, but never reads its data.
- **A convention for relay-like devices**, documented in `docs/plugin-contract.md`: report `switch.on` by default, and accept `parameters.capability` set to `light.on` or `outlet.on`. Reject any other value, and report it in `devices[].error`.
- **The SDK lives in this repo**, as a TypeScript workspace beside the Go code. *(Revised by [0006](0006-go-plugin-sdk.md).)* Plugin images build from here too, which brings a Node toolchain into CI.
- **Oikade ships a contract conformance suite**: a fake hub that drives a plugin through every topic. The Oikade side uses it in CI, and plugins built elsewhere can run it too.

What a user would commit for a plugin. SONOFF is shown for illustration; building that plugin isn't part of this spec.

```yaml
apiVersion: plugins.oikade.dev/v1alpha1
kind: Plugin
metadata:
  name: sonoff
spec:
  image: ghcr.io/harrymitchinson/oikade-plugin-sonoff:v0.1.0
  hostNetwork: true            # mDNS reports
  secretRef:
    name: sonoff-lan-keys      # one key per device id; restored from Git (SOPS)
---
apiVersion: home.oikade.dev/v1alpha1
kind: Device
metadata:
  name: porch-light
spec:
  roomRef: {name: porch}
  plugin:
    pluginRef: {name: sonoff}
    id: 1000aabbcc             # plugin-local and stable; the HomeKit AID still follows the Device name
    parameters:                # opaque to Oikade, validated by the plugin
      model: MINIR4
      capability: light.on     # default switch.on
```

## Non-goals

- Running third-party plugins from the internet. Plugins are trusted images the user builds. There's no sandboxing beyond the pod.
- Automation logic inside plugins. The pool scheduler's daily planning isn't expressible in CEL today, and whether it moves into Oikade, stays a plugin-internal feature, or becomes an engine feature is its own design.
- Real device plugins. The SONOFF MINIR4, TH Elite and Cozytouch plugins each get their own work item once the system exists. Where they live, and how they get `sonoff-lan`, is decided then.
- Plugin-owned persistent state (PVCs) in v1.
- Plugins defining new capabilities. New capabilities still go through `add-capability`.
- Discovery or adoption. Paired-but-undeclared devices count towards `Undeclared`, as they do for zigbee2mqtt.

---

## API

### New kind: `Plugin`

```sh
kubebuilder create api --group plugins --version v1alpha1 --kind Plugin --resource --controller
```

This creates `api/plugins/v1alpha1/plugin_types.go` and `internal/controller/plugins/plugin_controller.go`. The field style follows `api/zigbee/v1alpha1/zigbee2mqtt_types.go`.

```go
// PluginSpec declares a plugin process the operator runs. Devices reference it from spec.plugin.
type PluginSpec struct {
	// image is the plugin container image. It must speak the oikade/v1 contract.
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// hostNetwork runs the plugin on the node's network, for plugins that need mDNS or multicast.
	// +optional
	HostNetwork bool `json:"hostNetwork,omitempty"`

	// secretRef names a Secret mounted read-only at /etc/oikade/secrets. Plugins look keys up by device id.
	// The operator never owns, modifies or deletes it.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// nodeSelector constrains where the plugin runs.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations for the plugin pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// resources for the plugin container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`
}

// PluginStatus is the observed state of a Plugin.
type PluginStatus struct {
	// devices is how many Devices reference this plugin.
	// +optional
	Devices int32 `json:"devices,omitempty"`

	// configHash is the hash of the rendered device file the running pod was started with.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

Printer columns are `Image`, `Devices`, and `Ready` (`.status.conditions[?(@.type=='Ready')].status`).

The `Ready` reasons are:

| Reason | Status | Meaning |
|---|---|---|
| `Available` | True | The pod is available. |
| `Progressing` | False | The rollout hasn't finished yet. |
| `SecretNotFound` | False | `spec.secretRef` names a Secret that doesn't exist. The operator still deploys, because the plugin reports a per-device error for each missing key. |
| `DuplicateDevice` | False | Two Devices claim the same `id`. The first by name wins, as in `Zigbee2MQTTReconciler`. |
| `NameConflict` | False | `<plugin>-plugin` or `<plugin>-plugin-config` already exists and isn't owned by this Plugin, or `spec.secretRef` names the config Secret. Nothing is applied. |

### `Device.spec.plugin`

This goes in `api/home/v1alpha1/device_types.go`:

```go
// PluginDeviceSource identifies a device served by a Plugin.
type PluginDeviceSource struct {
	// pluginRef names the Plugin that serves the device.
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="pluginRef.name is required"
	// +required
	PluginRef corev1.LocalObjectReference `json:"pluginRef"`

	// id is the device's stable, plugin-local identifier, such as an eWeLink device ID. It appears in MQTT topics.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`
	// +required
	ID string `json:"id"`

	// parameters are plugin-specific settings. Oikade passes them through unchanged, and the plugin validates them.
	// Keys are up to 63 characters of letters, digits, '_', '.' and '-'; values are up to 1024 characters.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[A-Za-z0-9_.-]{1,63}$'))",message="parameter keys must be 1-63 characters of letters, digits, '_', '.' or '-'"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 1024)",message="parameter values must be at most 1024 characters"
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}
```

`DeviceSpec` gains `Plugin *PluginDeviceSource \`json:"plugin,omitempty"\``. Its rule changes from `has(self.zigbee2mqtt)` to:

```go
// +kubebuilder:validation:XValidation:rule="[has(self.zigbee2mqtt), has(self.plugin)].filter(x, x).size() == 1",message="exactly one driver source (zigbee2mqtt or plugin) is required"
```

Every existing Device still satisfies this rule. The `pluginRef.name` and parameter bounds were added in TASK-0001.03 review: an empty ref could never resolve, and unbounded values could push the rendered `<plugin>-plugin-config` Secret past 1 MiB.

### The `oikade/v1` contract

It's written up in full in `docs/plugin-contract.md`, which TASK-0001.01 creates, and enforced by the conformance suite. The root is `oikade/v1/<namespace>/<plugin>`.

| Topic | Direction | Retained | Payload |
|---|---|---|---|
| `status` | plugin → | yes | `{"state":"online","contract":1}`. The Last Will is `{"state":"offline","contract":1}`. |
| `devices` | plugin → | yes | `{"devices":[{"id","found","manufacturer","model","capabilities":[…],"error"}],"undeclared":N}` |
| `<id>/state` | plugin → | yes | The full current values, e.g. `{"switch.on":true}`. The hub diffs them. |
| `<id>/availability` | plugin → | yes | `online` or `offline` |
| `<id>/set` | → plugin | no | One capability per message, e.g. `{"switch.on":false}` |

The plugin learns its settings from environment variables and files that the operator sets:

- `OIKADE_MQTT_URL` and `OIKADE_TOPIC` (the root above)
- `OIKADE_DEVICES_FILE=/etc/oikade/config/devices.json`, containing `{"contract":1,"devices":[{"id","name","displayName","room","parameters"}]}`
- `OIKADE_SECRETS_DIR=/etc/oikade/secrets`

## Behaviour

### Manager: `PluginReconciler` (`internal/controller/plugins/plugin_controller.go`)

1. List the Devices in the namespace whose `spec.plugin.pluginRef.name` matches, sorted by name. Drop duplicate `id`s and remember them for `DuplicateDevice`.
2. Render `devices.json`, then apply Secret `<plugin>-plugin-config` with an owner reference, as `render.ZigbeeConfigSecret` does. The content is Device names and parameters. It's a Secret rather than a ConfigMap so that parameters which turn out to be sensitive aren't exposed, and it needs no new RBAC.
3. If `spec.secretRef` is set, `Get` it to decide `SecretNotFound`. The data is never read.
4. Apply Deployment `<plugin>-plugin` (`render.PluginDeployment`, a new function in `internal/render/plugin.go`). The `-plugin` suffix keeps both names clear of a Zigbee2MQTT with the same name (TASK-0001.05 review). If either object exists without this Plugin as its controller, or `spec.secretRef` names the config Secret, the operator applies nothing and reports `NameConflict`:
   - one replica, with the `Recreate` strategy, because two plugin pods would both answer `/set`
   - the `configHash` annotation, so a Device change restarts the pod
   - a `plugins.oikade.dev/secret-version` annotation holding the user Secret's `resourceVersion`, so rotating keys restarts the pod as well
   - the user Secret mounted `optional`, so a missing one shows as per-device errors rather than a pod stuck in `ContainerCreating`
   - `hostNetwork` and `dnsPolicy: ClusterFirstWithHostNet` when `hostNetwork` is set
   - `podSecurityContext` and `containerSecurityContext` from `render/common.go`
   - labels `plugins.oikade.dev/plugin: <name>`
5. Set `Ready` from the Deployment, as `deploymentAvailable` does.
6. Watch Devices through a mapping function like `instanceForDevice`. `Owns` covers the Deployment and the config Secret.

### Runtime and HAP: the hub (`internal/hub`) and the decoder (`internal/driver/plugin`)

1. **The hub becomes driver-agnostic.** `instances` changes to `map[InstanceKey]instance`.
   - `instance` is an internal interface: `Set`, `SetDevices(map[string]types.NamespacedName)` and `Stop`. Both `zigbee2mqtt.Instance` and `plugin.Instance` already have this shape.
   - `InstanceKey{Driver string; types.NamespacedName}` replaces the bare `NamespacedName` in `Meta.Instance`, `Store.SetDevices`, `Sink.Undeclared` and `UndeclaredCounts`. Without it, a Zigbee2MQTT and a Plugin with the same name in one namespace would collide in the store.
   - `Hub.Reconcile` splits into a Zigbee2MQTT reconciler (the current code) and a Plugin reconciler, both sharing `ensure` and `stop`. `SetupWithManager` registers both, so `cmd/runtime.go` and `cmd/hap.go` pick them up unchanged.
2. **`plugin.Instance.Start`** subscribes to `<root>/#`. On a message:
   - **`status`:** record up or down. If `contract` isn't 1, ignore everything from this plugin, count it as `wrong_contract`, and log once. When the plugin goes offline, send `Availability(dev, false)` for every declared device.
   - **`devices`:** for each declared id, send `Discovered` with `Info{Found, Capabilities, Manufacturer, Model}`. Capabilities not found by `capability.Lookup` are dropped and counted. A non-empty `error` goes into a new `driver.Info.Error` field (`internal/driver/driver.go`) for the status writer. `undeclared` goes to `Undeclared`.
   - **`<id>/state`:** if the id isn't declared, count it and stop. Otherwise send `Seen`, then for each key, run `capability.Validate`, diff against the last value, and send `Changed` if it differs. A capability the device didn't announce in `devices` is dropped and counted.
   - **`<id>/availability`:** send `Availability`.
3. **`plugin.Instance.Set`** publishes `{"<capability>": value}` to `<root>/<id>/set`, without retain. `Hub.Set` has already validated the value against the registry. The device must have announced the capability. There's no acknowledgement in v1, the same as Zigbee2MQTT: success shows up as a state change.
4. **The status writer** (`internal/status/writer.go` `compute`):
   - The `NoDriver` message names whichever source the Device uses.
   - A device whose `Info.Error` isn't empty gets `Resolved=False` with reason `PluginError`, and the plugin's message (truncated to 256 characters) as the condition message.

### Failure modes

| Situation | What happens | What the user sees |
|---|---|---|
| Broker down or restarted | `mqtt.Client` resubscribes. Retained `status`, `devices`, `state` and `availability` replay the current picture. | Nothing beyond the existing broker alerts. |
| Plugin pod crashes | The broker publishes the Last Will (`offline`). Every device goes `Available=False`. Kubernetes restarts the pod. | `oikade_plugin_up=0`, devices offline in HomeKit and in `oikade_device_available`. |
| Runtime restarts | Retained topics rebuild state. HAP keeps serving because it runs its own hub. | Automations pause, as they do today. |
| HAP pod restarts | Its hub replays retained topics before HomeKit reads. | Values return within seconds. |
| `pluginRef` names a missing Plugin | No hub instance claims the Device. | `Resolved=False NoDriver`. |
| Plugin deleted | The Deployment and config Secret are garbage-collected. The user's Secret and the Devices stay. | `Resolved=False NoDriver`. The HomeKit accessory stays with the same AID. |
| Bad parameters, or a missing key in the Secret | The plugin reports `error` for that id. | `Resolved=False PluginError: <message>`. |
| Unknown capability, out-of-range value or undeclared id | Dropped, never adopted. | `oikade_plugin_rejected_messages_total{reason}`, plus one log line per id and reason. |
| Duplicate `id` across Devices | The first by name wins, in both the manager and the hub. | The Plugin shows `Ready=False DuplicateDevice`, and the loser shows `NoDriver`. |
| Two plugin pods during a rollout | Can't happen: the strategy is `Recreate`. | |

## State and upgrade

- **What persists:** only the user's `spec.secretRef` Secret, which comes from Git via SOPS. The config Secret and the Deployment are rendered and owned, so losing them costs nothing. Plugins get no PVC in v1.
- **Upgrade:** purely additive. The new CRD, the new optional field, and a CEL rule every existing Device satisfies. The `InstanceKey` refactor is internal. `oikade_undeclared_devices{namespace,instance}` keeps its labels, with Zigbee2MQTT values unchanged, and Plugin values written as `plugin/<name>` so they can't collide.
- **Deleting a Plugin:** its Deployment and config Secret go. The user's Secret, the Devices and their AIDs stay.
- **Restoring an empty cluster from Git:** everything comes back by itself, because Plugin, Devices and the SOPS Secret are all in Git, and AIDs are restored by the existing `Bridge.status` mechanism.
- **Moving a device from Homebridge** gives it a new HomeKit accessory under an Oikade Bridge. The user re-adds it to rooms and scenes in the Home app once, and removes the Homebridge accessory.

## Observability

These are collected by the runtime in `internal/metrics/collector.go`:

- `oikade_plugin_up{namespace,plugin}` (gauge): 1 while the retained `status` is online with contract 1.
- `oikade_plugin_rejected_messages_total{namespace,plugin,reason}` (counter). `reason` is one of `malformed`, `wrong_contract`, `unknown_capability`, `invalid_value`, `undeclared_device` or `unannounced_capability`.
- The existing device and capability metrics cover plugin devices without changes.

The log messages are:

- `"Rejected plugin message", "plugin", key, "reason", r, "topic", t`, rate-limited to once per plugin, reason and id
- `"Plugin went offline"` and `"Plugin came online"`

Conditions are `Plugin` `Ready`, and `Device` `Resolved=False PluginError`. The Grafana device panels are unchanged. A "Plugins" row shows `oikade_plugin_up` and the rate of rejected messages.

## Security

- **Manager RBAC** (markers on `PluginReconciler`): `plugins.oikade.dev` `plugins` get, list and watch; status get, update and patch; finalizers update; Devices get, list and watch. The existing secrets and deployments verbs already cover the rest.
- **Runtime and HAP:**
  - `config/runtime/role.yaml` gains read access to `plugins` in `plugins.oikade.dev`.
  - The HAP Role in `internal/render/hap.go` (next to `zigbee2mqtts`) gains the same, read-only.
  - HAP still reads only its own Secrets.
- **The plugin pod** has no service account token (`automountServiceAccountToken: false`). It runs as non-root with a read-only root filesystem and all capabilities dropped. With `hostNetwork` it shares the node's network. It only makes outbound connections and joins mDNS.
- **The broker is still anonymous.** Anything on the cluster network can spoof `oikade/v1/...`, exactly as it can spoof `zigbee2mqtt/...` today. MQTT auth remains its own "Not yet" item, and when it lands it should give each plugin a credential scoped to its own root.
- **LAN keys** stay in the user's Secret and the pod's mounted files. The SDK never logs `OIKADE_SECRETS_DIR` contents.

## Test plan

| Behaviour | Layer | Test |
|---|---|---|
| Decoder: `devices` gives `Discovered`, dropping unknown capabilities | Unit | `internal/driver/plugin` `TestDevicesDropsUnknownCapability` with `mqtt.Memory` |
| Decoder: `state` diffs give `Changed`, invalid values are rejected | Unit | `TestStateChangesAndRejects` |
| Decoder: an offline `status` marks every device unavailable, a wrong contract is ignored | Unit | `TestStatusOfflineAndContract` |
| `Set` publishes one capability to `<id>/set`, refusing unannounced capabilities | Unit | `TestSetPublishes` |
| Zigbee2MQTT and Plugin with the same name don't collide | Unit | `internal/hub` `TestInstanceKeySeparatesDrivers` |
| Bus → status, metrics and engine for a plugin device | Integration | `internal/runtime` with `mqtttest`: publish retained topics, assert `Device.status`, metrics and an Automation `/set` |
| CEL: exactly one source, `id` pattern | envtest | `internal/controller/home` creates valid and invalid Devices |
| Rendered config Secret, Deployment shape (`hostNetwork`, `Recreate`, hash) | envtest and unit | `internal/controller/plugins` suite, plus `internal/render` `TestPluginDeployment` |
| `Ready` reasons `SecretNotFound`, `DuplicateDevice`, `Available` | envtest | plugin controller suite |
| SDK publishes a conforming contract, honours `/set` | Conformance (CI) | `test/conformance` against `plugins/example-switch` (simulated) |
| End to end: Plugin and Device deploy, a faked plugin feeds state | Kind e2e | `mosquitto_pub` to `oikade/v1/...`, assert `Device` `Resolved=True` and the metric |

Manual checks on the Pi:

1. **The example plugin runs on arm64.** Pass: `oikade_plugin_up{plugin="example-switch"}=1`, and RSS under 128 MiB after an hour.
2. **HomeKit control.** Pass: toggling the example Device in the Home app shows `oikade/v1/<ns>/example-switch/<id>/set` on the broker within 1 s, and the simulated state follows.
3. **The runtime is down.** Pass: after `kubectl scale deploy/oikade-runtime --replicas=0`, the Home app toggle still works.

## Docs

- **README:**
  - add `Plugin` to the kinds and Components tables
  - add the two metrics to Metrics
  - change "Drivers other than Zigbee2MQTT" under "Not yet" to "Built-in decoders for MQTT-native firmware (ESPHome, Tasmota)"
  - add a short "Plugins" section linking to the contract
- **`docs/plugin-contract.md`** (new): the topics, payloads, environment, contract versioning, and conformance.
- **`config/samples/plugins_v1alpha1_plugin.yaml`**, and a plugin variant of `home_v1alpha1_device.yaml`.
- **`docs/deploy-raspberry-pi.md`**: running a plugin, `hostNetwork`, and a SOPS Secret for plugin credentials.
- **"State that must survive"**: add the plugin Secret, noting it's user-owned and comes from Git.
- **AGENTS.md**: add the TypeScript workspace commands.

## Open questions

None.
