# 0006: Go plugin SDK

> Revises [0002](0002-plugin-drivers.md), where an agent chose to put a TypeScript SDK in this repo without asking you. Work: [#30](https://github.com/harrymitchinson/oikade/issues/30).

## Problem

[0002](0002-plugin-drivers.md) made plugins language-neutral: a plugin is any process that speaks five MQTT topics with JSON payloads, and reads its settings from environment variables and a devices file. Oikade then shipped a handwritten TypeScript SDK and example plugin in this repo (`sdk/typescript`, `plugins/example-switch`, #12). With them came a Yarn workspace, a Node CI job and a Node-based `plugins/Dockerfile`.

That's the wrong shape:

- **Go should be the first plugin language.** It's your language and Oikade's, and a plugin is a ~10 MiB static arm64 binary on the Pi, against 60–100 MiB for Node. A Go plugin in this repo reuses Oikade's MQTT client, linting and image builds, and adds no toolchain.
- **Handwritten SDKs in other languages don't belong here.** That's your rule. Another language's SDK may live in this repo only if it's generated from the contract.
- **The contract has two Go implementations with no shared types.** Oikade's decoder (`internal/driver/plugin`) has its own payload structs, and a Go SDK would need another set. Nothing would keep the two in step except the conformance suite.

Done looks like this:

- A Go plugin in this repo implements one interface and passes conformance.
- Oikade's decoder and the Go SDK compile against the same contract types.
- Node is gone from this repo.

## Constraints

- **Public API.** The topics and payloads are unchanged, so it's still `oikade/v1`. Two Go packages become public API, so their exported names matter: `contract/v1` (the payload types) and `sdk/plugin` (the SDK).
- **Capabilities stay owned by `internal/capability`.** The SDK checks values against the same registry the decoder uses. A plugin can't define new capabilities ([0002](0002-plugin-drivers.md)).
- **Component boundaries.** Plugins are still separate pods and separate binaries. Living in the same module doesn't make them part of `manager`. Oikade still validates everything a plugin sends.
- **Pi.** Go plugin images are static arm64 builds, from the same distroless base as the manager.

## Options

### A. Go types are the contract, with no generator yet

- **`contract/v1`:** handwritten Go types for every payload and the devices file, plus topic builders. `internal/driver/plugin` switches to them.
- **`sdk/plugin`:** the Go SDK, a handwritten runtime over `contract/v1`. It reads the environment and devices file, connects with the Last Will, publishes retained `status`, `devices`, `state` and `availability`, dispatches `/set`, republishes after a reconnect and exits cleanly on SIGTERM.
- **`plugins/example-switch`:** rewritten in Go. CI's `make test-conformance` runs against it.
- **Removed:** `sdk/typescript`, the TypeScript `example-switch`, the Yarn workspace, the Node CI job and the Node `plugins/Dockerfile`.
- **`docs/plugin-contract.md`** stays the readable spec, and `test/conformance` stays what enforces it.

Trade-offs:

- Pros:
  - It's the least machinery that removes the drift: one set of types, two users, both in Go.
  - There's no generator, no AsyncAPI file and no schema to keep in step by hand.
- Cons:
  - A plugin in another language has only the prose and the conformance suite to work from, and the suite still needs an Oikade checkout.
  - #12's TypeScript SDK is deleted.
- Cost of reversing: low. Adding a schema later is additive: it's derived from these types.

### B. Go types now, and a JSON Schema derived from them

As A, plus `make generate` writes `contract/v1/schema.json` from the Go types and `internal/capability`. That's Go → schema, the way controller-gen already writes CRDs. A test fails if the two drift. `test/conformance` would also be built as a standalone `oikade-conformance` binary.

- Pros: other languages could generate their types and check themselves today. It's also the base for any generated SDK this repo might hold later.
- Cons: it builds two things for a consumer that doesn't exist yet. No non-Go plugin is planned.
- Cost of reversing: low.

### C. Schema first, with code generated from it

Write AsyncAPI and JSON Schemas by hand, then generate the Go types from them (`go-jsonschema` or Modelina).

- Pros: it's the conventional spec-first shape.
- Cons: the schema becomes a second handwritten artefact that every Go change goes through. It's backwards for a repo where Go types are already the source of truth for CRDs. Modelina would bring Node back.
- Cost of reversing: medium.

## Decision

**Accepted: A.** Every user of the contract in this repo is Go, so shared Go types solve the drift on their own, and a generator would serve nobody yet. B is the planned next step, not a rejected option. Do it when the first non-Go plugin is planned, or when someone wants a generated SDK in this repo. It's additive, because the schema is derived from the types A creates. What A gives up: until then, a plugin in another language works from `docs/plugin-contract.md` and an Oikade checkout.

The order of SDKs:

1. **Go:** here, now, handwritten.
2. **Any other language:** none maintained here unless it's generated, which needs B first. A plugin in another language lives in its own repo.

The new public names:

- **The Go packages** `github.com/harrymitchinson/oikade/contract/v1` and `github.com/harrymitchinson/oikade/sdk/plugin`.
- **Plugin images** `ghcr.io/harrymitchinson/oikade-plugin-<name>`, unchanged and built by `make docker-buildx-plugin PLUGIN=<name>`, now from a Go Dockerfile.

What a Go plugin looks like: see API, below.

## Non-goals

- Changing the `oikade/v1` topics or payloads.
- A JSON Schema, AsyncAPI or any code generator (option B is deferred until a non-Go consumer exists).
- SDKs in any language other than Go.
- Building any real device plugin. Those are separate work items, in whatever language suits them.

## Open questions

None.

Settled in review:

- **Home Assistant:** what's ruled out is HA as a driver. Standalone device libraries in any language are allowed.
- **Tracking:** this is a new piece of work ([#30](https://github.com/harrymitchinson/oikade/issues/30)), separate from TASK-0001.06.
- **Go is the first plugin language, and the Go SDK and Go plugins live in this repo.** Other languages' SDKs may live here only if they're generated.
- **No generator yet.** Shared Go types are enough while every user of the contract is Go.
- **No vendor drives this design.** Real device plugins are separate work.

---

## API

No CRD changes: `Plugin`, `Device.spec.plugin`, the topics and the payloads all stay as they are. The new API is two Go packages. Both live in the main module, so they can import `internal/capability` and `internal/mqtt`, but their exported signatures must not mention internal types, because an importer outside the tree can't name them.

### `contract/v1` (package `v1`, imported as `contractv1`)

This is the wire contract as Go types, moved out of `internal/driver/plugin` (`statusMessage`, `devicesMessage`) and `internal/render/plugin.go` (`PluginDevice`, `pluginConfig`). The JSON tags are the ones in use today, so the bytes on the wire and in `devices.json` don't change.

```go
// Package v1 is the oikade/v1 plugin contract (docs/plugin-contract.md) as Go types.
package v1

const Contract = 1

// The values of the status and availability topics.
const (
	Online  = "online"
	Offline = "offline"
)

// The environment the operator sets on a plugin pod.
const (
	EnvMQTTURL     = "OIKADE_MQTT_URL"
	EnvTopic       = "OIKADE_TOPIC"
	EnvDevicesFile = "OIKADE_DEVICES_FILE"
	EnvSecretsDir  = "OIKADE_SECRETS_DIR"
)

// Root is a plugin's topic root, oikade/v1/<namespace>/<plugin>.
type Root string

func NewRoot(namespace, plugin string) Root
func ParseRoot(s string) (Root, error) // rejects anything but oikade/v1/<ns>/<plugin> without + # /
func (r Root) Status() string
func (r Root) Devices() string
func (r Root) State(id string) string
func (r Root) Availability(id string) string
func (r Root) Set(id string) string
func (r Root) SetFilter() string // <root>/+/set
func (r Root) Filter() string    // <root>/#

// Status is the retained status payload, and the Last Will when State is Offline.
type Status struct {
	State    string `json:"state"`
	Contract int    `json:"contract"`
}

// Devices is the retained devices payload.
type Devices struct {
	Devices    []DeviceReport `json:"devices"`
	Undeclared int            `json:"undeclared"`
}

type DeviceReport struct {
	ID           string   `json:"id"`
	Found        bool     `json:"found"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// State is a device's full current values: bool, or float64 within the capability's range.
type State map[string]any

// SetCommand is one capability of one device, as sent on <root>/<id>/set.
type SetCommand struct {
	ID         string
	Capability string
	Value      any
}

func (c SetCommand) Payload() ([]byte, error) // {"<capability>": value}
// ParseSet parses a message on <root>/<id>/set. ok is false for any other topic; err is set for a
// payload that isn't exactly one capability with a bool or number value.
func (r Root) ParseSet(topic string, payload []byte) (cmd SetCommand, ok bool, err error)

// DevicesFile is devices.json, written by the operator and read by the plugin.
type DevicesFile struct {
	Contract int              `json:"contract"`
	Devices  []DeclaredDevice `json:"devices"`
}

type DeclaredDevice struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName"`
	Room        string            `json:"room,omitempty"`
	Parameters  map[string]string `json:"parameters,omitempty"`
}
```

Its users:

- `internal/driver/plugin`: it uses `Status`, `Devices`, `Root` and `SetCommand.Payload`, and its private message structs go.
- `internal/render/plugin.go`: it uses `DevicesFile`, `DeclaredDevice` and the `Env*` constants. `render.PluginDevice` goes.
- `api/plugins/v1alpha1` `(*Plugin).Topic()`: it returns `string(contractv1.NewRoot(p.Namespace, p.Name))`. This creates the import edge api → contract, so `contract/v1` must import nothing from `api/` or `internal/`.
- `test/conformance`.

### `sdk/plugin` (package `plugin`)

This is a Go port of `sdk/typescript/src/plugin.ts` and `environment.ts`. The behaviour is the same, but values are checked against `internal/capability` before they're published.

```go
// Package plugin is the Go SDK for Oikade plugins.
package plugin

type Environment struct {
	MQTTURL     string
	Root        contractv1.Root
	DevicesFile string
	SecretsDir  string // empty when the Plugin has no spec.secretRef
}

func ReadEnvironment() (Environment, error) // from the Env* variables; names the missing one
func ReadDevices(file string) ([]contractv1.DeclaredDevice, error) // checks contract == 1
// ReadSecret reads one key of the mounted Secret. ok is false without a Secret or the key.
// Keys containing '/' or equal to "." or ".." are refused. Never log the result.
func ReadSecret(env Environment, key string) (value string, ok bool, err error)
// RelayCapability picks switch.on, or parameters["capability"] when it is light.on or outlet.on.
func RelayCapability(parameters map[string]string) (string, error)

type Options struct {
	Environment  Environment
	OnSet        func(ctx context.Context, p *Plugin, cmd contractv1.SetCommand) error
	Logger       *slog.Logger  // default: JSON to stderr
	StartTimeout time.Duration // default 60s: a wrong broker URL crashes the pod, not leaves it silent
	CloseTimeout time.Duration // default 2s
}

func Start(ctx context.Context, o Options) (*Plugin, error)
func (p *Plugin) SetDevices(ctx context.Context, devices []contractv1.DeviceReport, undeclared int) error
func (p *Plugin) SetState(ctx context.Context, id string, s contractv1.State) error    // full state
func (p *Plugin) UpdateState(ctx context.Context, id string, s contractv1.State) error // merge
func (p *Plugin) SetAvailability(ctx context.Context, id string, online bool) error
func (p *Plugin) State(id string) contractv1.State
func (p *Plugin) Close(ctx context.Context) error

// Handler is a plugin's device logic.
type Handler interface {
	// Serve reports devices, availability and state, then follows the devices until ctx ends.
	Serve(ctx context.Context, p *Plugin) error
	// Set applies a command and reports the result with SetState or UpdateState.
	Set(ctx context.Context, p *Plugin, cmd contractv1.SetCommand) error
}

// Run is a plugin's main: it reads the environment and devices file, builds the handler,
// starts, serves, and on SIGTERM or SIGINT closes and exits 0. Any error exits 1.
func Run(newHandler func(env Environment, devices []contractv1.DeclaredDevice) (Handler, error))
```

This replaces the sketch in the Decision above. Building one handler from the whole device list fits real plugins better than a per-device factory, because they usually share one LAN listener or cloud session across all their devices.

A Go plugin's whole `main`:

```go
// plugins/example-switch/main.go
func main() {
	plugin.Run(func(_ plugin.Environment, devices []contractv1.DeclaredDevice) (plugin.Handler, error) {
		return newSwitches(devices), nil // simulated relays, one per declared device
	})
}
```

The user's YAML doesn't change. A Plugin points at the new image tag:

```yaml
apiVersion: plugins.oikade.dev/v1alpha1
kind: Plugin
metadata: {name: example-switch}
spec:
  image: ghcr.io/harrymitchinson/oikade-plugin-example-switch:v0.2.0   # now a ~10 MiB Go image
```

### `internal/mqtt` additions

`Connect` stays source-compatible, and gains options:

```go
type Option func(*options)
func WithWill(topic string, payload []byte) Option // retained, QoS 1 (autopaho WillMessage)
func WithOnConnect(f func(ctx context.Context)) Option // every connection, after subscriptions are restored
func Connect(ctx context.Context, server, clientID string, opts ...Option) (*Client, error)
// Disconnect sends a clean DISCONNECT, so the broker doesn't publish the Will.
func (c *Client) Disconnect(ctx context.Context) error
```

## Behaviour

Everything here runs in the plugin pod. The manager, runtime and hap pods behave exactly as before, with types moved but no logic changed.

1. **`Run`** reads the environment and the devices file and calls `newHandler`. An error at this point is logged and exits 1, so the pod shows CrashLoopBackOff with the reason in its logs.
2. **`Start`** connects with a random client ID (`oikade-plugin-<12 hex>`) and the Last Will `Status{Offline, 1}` on `Root.Status()`, retained. The ID has to be random: a takeover by a fixed ID would fire the old session's Will after the new one announced itself online. `Start` subscribes to `Root.SetFilter()`.
3. **On every connection**, through `WithOnConnect`, the SDK announces in this order:
   1. Publish `Status{Online, 1}`.
   2. Publish `devices`, if `SetDevices` has run. Until then, whatever is retained stays: an empty list would mark every Device not found while the plugin starts.
   3. Publish every known availability and state.

   If `Close` has started, nothing is announced, so an `online` can never land after the `offline`. `Start` returns once the first announcement completes, or fails after `StartTimeout`.
4. **`SetDevices`** stores the reports and publishes them. Values a device no longer announces are dropped from its stored state, and that state is republished, so a restarting hub doesn't read and reject them.
5. **`SetState` and `UpdateState`** return an error, and publish nothing, for an id that isn't found or a capability it hasn't announced. They do the same for a value that `capability.Validate` rejects. This is stricter than the TypeScript SDK: a mistake is the plugin's error, not an Oikade rejection counter.
6. **`/set` handling:**
   - `Root.ParseSet` parses the message. A malformed message is logged as `"Ignored set command"` with `topic` and `reason`, and dropped.
   - So is a capability the device hasn't announced.
   - Otherwise the SDK calls `OnSet`, with handler calls serialised per device. An error from `OnSet` is logged as `"Could not apply set command"`.
   - Nothing is acknowledged: success is the state changing (contract v1).
7. **SIGTERM, or `Close`:**
   1. Publish `Status{Offline, 1}`, retained, within `CloseTimeout`.
   2. `Disconnect`, then exit 0.

   If the publish times out, the SDK drops the connection without a DISCONNECT, which leaves it to the broker to publish the Will.

### Failure modes

| Situation | What happens | What the user sees |
|---|---|---|
| Wrong `OIKADE_MQTT_URL` or the broker is down at start | `Start` fails after 60 s, and `Run` exits 1. | The pod is in CrashLoopBackOff, with `"Could not start plugin"` in its logs. The Plugin shows `Ready=False Progressing`. |
| Broker restarts later | autopaho reconnects, subscriptions are restored, and `OnConnect` re-announces everything. | Devices are briefly unavailable, then recover by themselves. |
| Plugin killed (OOM, node loss) | The broker publishes the Will. | `oikade_plugin_up=0`, and every Device goes `Available=False`. |
| Bad devices file or missing environment | Exit 1, with the reason. | CrashLoopBackOff, with the reason in the logs. |
| Handler publishes an unannounced capability or an out-of-range value | `SetState` returns an error, and nothing is published. | The plugin's own log. `oikade_plugin_rejected_messages_total` doesn't move. |
| `OnSet` fails (for example, the device is unreachable) | It's logged, and the state is unchanged. | The Home app reverts the toggle once state doesn't follow. |

## State and upgrade

- **What persists:** nothing new. Plugins still get no PVC ([0002](0002-plugin-drivers.md)).
- **Upgrade:**
  - The contract is unchanged, so existing plugin images, including the TypeScript `example-switch:v0.1.0`, keep working with the new Oikade.
  - `devices.json` is rendered byte-for-byte as before (a golden test checks this), so the `configHash` doesn't change and no plugin pod restarts on an operator upgrade.
  - Moving `example-switch` to Go is just a new image tag.
  - The pod's security context (UID 1000, non-root, read-only root filesystem) suits a static binary unchanged.
- **Removing the TypeScript SDK:** nothing at runtime depends on it. The npm package was never published, so there are no external users.
- **Restoring an empty cluster from Git:** unchanged.

## Observability

- **No new metrics.** `oikade_plugin_up` and `oikade_plugin_rejected_messages_total` in the runtime already cover every plugin. Plugins don't serve `/metrics`.
- **SDK log messages,** as JSON on stderr through `log/slog`. They keep the TypeScript SDK's wording, so Loki queries carry over:
  - `"Plugin came online"`, with `topic`
  - `"Ignored set command"`, with `topic` and `reason`
  - `"Could not apply set command"`, with `topic`, `capability` and `error`
  - `"MQTT connection failed"`, with `error`
  - `"Could not start plugin"`, with `error`
  - `"Could not close plugin"`, with `error`
- **Conditions, events and Grafana:** unchanged.

## Security

- **RBAC:** unchanged. No Kubernetes API access is added. The plugin pod keeps `automountServiceAccountToken: false`.
- **Image:** `gcr.io/distroless/static:nonroot`, with no shell. It runs as UID 1000 from the pod security context.
- **Secrets:** `ReadSecret` refuses path-like keys and never logs values. The SDK never logs the contents of `SecretsDir`.
- **Network:** unchanged. Connections are outbound to the broker, and the broker is still anonymous ([#41](https://github.com/harrymitchinson/oikade/issues/41)).

## Test plan

| Behaviour | Layer | Test |
|---|---|---|
| Contract types round-trip today's payloads byte-for-byte | Unit | `contract/v1` `TestPayloadsMatchContractDoc`, using the examples in `docs/plugin-contract.md` |
| `Root` builders, `ParseRoot` rejections, and `ParseSet` (one capability, bool or number, other topics ignored) | Unit | `contract/v1` `TestRoot`, `TestParseSet` |
| The decoder behaves the same on the shared types | Unit | the existing `internal/driver/plugin` tests, unchanged and passing |
| `devices.json` is rendered byte-identically | Unit | `internal/render` `TestPluginConfigUnchanged` (golden) |
| The Will is published when the connection drops, but not on `Disconnect` | Integration | `internal/mqtt` with `mqtttest`: `TestWillOnDrop`, `TestNoWillOnDisconnect` |
| `OnConnect` runs on every connection, after subscriptions are restored | Integration | `internal/mqtt` `TestOnConnectAfterResubscribe`, restarting the broker |
| The SDK announces in order, republishes after a reconnect, and never publishes `online` after `Close` | Integration | `sdk/plugin` with `mqtttest`: `TestAnnounceOrder`, `TestReconnectRepublishes`, `TestCloseWins` |
| `SetState` refuses unannounced capabilities and invalid values | Unit | `sdk/plugin` `TestSetStateValidates` |
| `ReadEnvironment`, `ReadDevices`, `ReadSecret` and `RelayCapability` | Unit | `sdk/plugin` table tests, ported from `sdk/typescript/test` |
| The Go `example-switch` passes the full contract suite | Conformance (CI) | `make test-conformance` with `CONFORMANCE_PLUGIN=bin/example-switch` |
| Plugin images build for arm64 and amd64 | CI | the `docker-buildx-plugin PLUGIN=example-switch` job |
| Plugin Devices in a cluster | Kind e2e | the existing `test/e2e` plugin case, unchanged |

These are checked manually on the Pi:

1. **The Go `example-switch` on arm64.** Pass: `oikade_plugin_up{plugin="example-switch"}=1`, and RSS under 32 MiB after an hour (`kubectl top pod`).
2. **Toggling in the Home app.** Pass: `oikade/v1/<ns>/example-switch/<id>/set` appears on the broker within 1 s, and the switch's state follows.
3. **Rolling the plugin.** Pass: `kubectl rollout restart deploy/example-switch` leaves the Device `Available` again within 30 s, with no rejected messages.

## Docs

- **`docs/plugin-contract.md`:**
  - Point "Conformance" at a Go plugin command.
  - Replace "Plugins written in TypeScript can use the SDK" with the Go SDK and `contract/v1`.
  - Say that plugins in other languages implement the prose contract and pass conformance from an Oikade checkout.
- **README, "Plugins":**
  - The Go SDK replaces the TypeScript one.
  - Development: remove `make test-plugins` and the Node requirement.
- **AGENTS.md:**
  - Remove the "TypeScript workspace" section.
  - Add `contract/v1` and `sdk/plugin` to Project Structure.
  - The rule becomes: change `docs/plugin-contract.md`, `contract/v1`, the decoder and `test/conformance` together.
- **`sdk/plugin/doc.go`:** a package overview with the `example-switch` `main`.
- **`plugins/example-switch/README.md`:** rewrite it for Go.
- **`docs/deploy-raspberry-pi.md`, "Run plugins":** the image is now Go, and the arm64 notes for Node go.
- **`config/samples/plugins_v1alpha1_plugin.yaml`:** bump the image tag when it's released.
- **`.github/workflows/test-e2e.yml`:** add `contract/**` to `paths`, because the decoder depends on it.
