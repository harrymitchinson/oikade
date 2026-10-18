import {
  jsonLogger,
  Plugin,
  readDevices,
  readEnvironment,
} from "@harrymitchinson/oikade-plugin-sdk";

import { apply, plan, stateOf } from "./switches.js";

const environment = readEnvironment();
const { switches, reports } = plan(await readDevices(environment.devicesFile));

const plugin = await Plugin.start({
  environment,
  onSet: async (command, running) => {
    const result = apply(switches, command);
    if ("error" in result) {
      throw new Error(result.error);
    }
    await running.setState(command.id, result.state);
  },
});

await plugin.setDevices(reports);
for (const target of switches.values()) {
  await plugin.setAvailability(target.id, true);
  await plugin.setState(target.id, stateOf(target));
}
jsonLogger.info("Serving simulated switches", { switches: switches.size });

for (const signal of ["SIGTERM", "SIGINT"] as const) {
  process.once(signal, () => {
    plugin.close().then(
      () => process.exit(0),
      (error: unknown) => {
        jsonLogger.error("Could not close plugin", { error: String(error) });
        process.exit(1);
      }
    );
  });
}
