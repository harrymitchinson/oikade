# Oikade plugin SDK for TypeScript

`@harrymitchinson/oikade-plugin-sdk` handles the [oikade/v1 contract](../../docs/plugin-contract.md) for a plugin, so the plugin only talks to its devices.

It:

- reads the environment the operator sets (`OIKADE_MQTT_URL`, `OIKADE_TOPIC`, `OIKADE_DEVICES_FILE`, `OIKADE_SECRETS_DIR`)
- reads and checks `devices.json`
- connects with a retained offline Last Will and publishes `status` online
- publishes `devices`, `<id>/state` and `<id>/availability` retained, and publishes them again after every reconnect
- checks each `<id>/set` command against the device's announced capabilities before handing it to the plugin
- publishes `status` offline on a clean shutdown, which doesn't trigger the Last Will

## Use

```ts
import {
  Plugin,
  readDevices,
  readEnvironment,
  readSecret,
  relayCapability,
} from "@harrymitchinson/oikade-plugin-sdk";

const environment = readEnvironment();
const declared = await readDevices(environment.devicesFile);

const plugin = await Plugin.start({
  environment,
  onSet: async ({ id, capability, value }, running) => {
    await myDevices.get(id).set(capability, value);
    // There's no acknowledgement in v1: report the result as state.
    await running.updateState(id, { [capability]: value });
  },
});

await plugin.setDevices(
  declared.map((d) => {
    const chosen = relayCapability(d.parameters);
    if ("error" in chosen) {
      return { id: d.id, found: false, error: chosen.error };
    }
    return {
      id: d.id,
      found: true,
      model: "MINIR4",
      capabilities: [chosen.capability],
    };
  })
);
await plugin.setAvailability("1000aabbcc", true);
await plugin.setState("1000aabbcc", { "light.on": true });

process.once("SIGTERM", () => void plugin.close().then(() => process.exit(0)));
```

- **`setDevices`:** list every declared device. Report a configuration problem, such as a bad parameter or a missing key, as `error`.
- **`setState`:** replaces a device's full state. `updateState` merges values into it. Both refuse capabilities the device hasn't announced in `setDevices`.
- **`readSecret`:** returns one key of the mounted Secret, by convention the device id. Never log what it returns.
- **`relayCapability`:** implements the relay convention, `switch.on` by default or `light.on` or `outlet.on` from `parameters.capability`.

[`plugins/example-switch`](../../plugins/example-switch) is a complete plugin with simulated relays.

## Develop

From the repository root:

```sh
make test-plugins       # format check, lint, typecheck and unit tests
make test-conformance   # run the conformance suite against example-switch
```
