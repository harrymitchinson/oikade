# example-switch

A plugin with simulated relays, so the oikade/v1 contract can be tested without hardware. Every declared Device becomes a relay that starts off and follows `/set`. It follows the [relay convention](../../docs/plugin-contract.md#relay-like-devices): `switch.on` by default, or `light.on` or `outlet.on` from `parameters.capability`. Any other value is reported as a device error.

It's written in Go with the [plugin SDK](../../sdk/plugin): `main.go` calls `plugin.Run`, and `switches.go` is the handler. Copy it to start a new plugin in this repo.

The conformance suite runs against it in CI.

```sh
make test-conformance                                   # from the repository root
make docker-buildx-plugin PLUGIN=example-switch         # linux/arm64 and linux/amd64 image
```

The image is a static binary on `gcr.io/distroless/static:nonroot`.

```yaml
apiVersion: plugins.oikade.dev/v1alpha1
kind: Plugin
metadata:
  name: example-switch
spec:
  image: ghcr.io/harrymitchinson/oikade-plugin-example-switch:dev
```
