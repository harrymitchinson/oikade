import {
  jsonLogger,
  Plugin,
  readDevices,
  readEnvironment,
} from "@harrymitchinson/oikade-plugin-sdk";

import { apply, plan, stateOf } from "./switches.js";

const running: { plugin?: Plugin } = {};

// Installed first, so a stop during startup still exits cleanly. Before the plugin is online
// there's nothing to mark offline.
for (const signal of ["SIGTERM", "SIGINT"] as const) {
  process.once(signal, () => {
    const { plugin } = running;
    if (plugin === undefined) {
      process.exit(0);
    }
    plugin.close().then(
      () => process.exit(0),
      (error: unknown) => {
        jsonLogger.error("Could not close plugin", { error: String(error) });
        process.exit(1);
      }
    );
  });
}

const environment = readEnvironment();
const { switches, reports } = plan(await readDevices(environment.devicesFile));

const plugin = await Plugin.start({
  environment,
  onSet: async (command, started) => {
    const result = apply(switches, command);
    if ("error" in result) {
      throw new Error(result.error);
    }
    await started.setState(command.id, result.state);
  },
});

running.plugin = plugin;

await plugin.setDevices(reports);
for (const target of switches.values()) {
  await plugin.setAvailability(target.id, true);
  await plugin.setState(target.id, stateOf(target));
}
jsonLogger.info("Serving simulated switches", { switches: switches.size });
