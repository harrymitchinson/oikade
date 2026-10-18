/** The capabilities a relay-like device may report (docs/plugin-contract.md, "Relay-like devices"). */
export const RELAY_CAPABILITIES = [
  "switch.on",
  "light.on",
  "outlet.on",
] as const;

export type RelayCapability = (typeof RELAY_CAPABILITIES)[number];

/**
 * Picks the capability a relay reports from its `parameters.capability`: `switch.on` by default,
 * or `light.on` or `outlet.on`. Any other value is an error to report in `devices`.
 */
export function relayCapability(
  parameters: Record<string, string>
): { capability: RelayCapability } | { error: string } {
  const wanted = parameters.capability;
  if (wanted === undefined || wanted === "") {
    return { capability: "switch.on" };
  }
  const match = RELAY_CAPABILITIES.find((c) => c === wanted);
  if (match === undefined) {
    return {
      error: `parameters.capability must be one of ${RELAY_CAPABILITIES.join(", ")}, got ${wanted}`,
    };
  }
  return { capability: match };
}
