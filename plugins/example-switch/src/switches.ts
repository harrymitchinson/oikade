import {
  type DeclaredDevice,
  type DeviceReport,
  type RelayCapability,
  relayCapability,
  type SetCommand,
  type State,
} from "@harrymitchinson/oikade-plugin-sdk";

/** A simulated relay. It starts off and follows /set, as a real relay follows its wall switch. */
export interface Switch {
  id: string;
  capability: RelayCapability;
  on: boolean;
}

/** Turns the declared devices into simulated switches and the devices report. */
export function plan(declared: DeclaredDevice[]): {
  switches: Map<string, Switch>;
  reports: DeviceReport[];
} {
  const switches = new Map<string, Switch>();
  const reports: DeviceReport[] = [];
  for (const device of declared) {
    const chosen = relayCapability(device.parameters);
    if ("error" in chosen) {
      reports.push({ id: device.id, found: false, error: chosen.error });
      continue;
    }
    switches.set(device.id, {
      id: device.id,
      capability: chosen.capability,
      on: false,
    });
    reports.push({
      id: device.id,
      found: true,
      manufacturer: "Oikade",
      model: "Simulated switch",
      capabilities: [chosen.capability],
    });
  }
  return { switches, reports };
}

/** Applies a command to a switch and returns its new state, or an error. */
export function apply(
  switches: Map<string, Switch>,
  command: SetCommand
): { state: State } | { error: string } {
  const target = switches.get(command.id);
  if (target === undefined || target.capability !== command.capability) {
    return { error: `no switch ${command.id} with ${command.capability}` };
  }
  if (typeof command.value !== "boolean") {
    return { error: `${command.capability} must be a boolean` };
  }
  target.on = command.value;
  return { state: stateOf(target) };
}

export function stateOf(target: Switch): State {
  return { [target.capability]: target.on };
}
