import type { DeclaredDevice } from "@harrymitchinson/oikade-plugin-sdk";
import { describe, expect, it } from "vitest";

import { apply, plan } from "../src/switches.js";

const declared: DeclaredDevice[] = [
  { id: "a", name: "hall", displayName: "Hall", parameters: {} },
  {
    id: "b",
    name: "porch",
    displayName: "Porch",
    parameters: { capability: "light.on" },
  },
  {
    id: "c",
    name: "fan",
    displayName: "Fan",
    parameters: { capability: "fan.on" },
  },
];

describe("plan", () => {
  it("follows the relay convention", () => {
    const { switches, reports } = plan(declared);
    expect([...switches.keys()]).toEqual(["a", "b"]);
    expect(reports).toEqual([
      {
        id: "a",
        found: true,
        manufacturer: "Oikade",
        model: "Simulated switch",
        capabilities: ["switch.on"],
      },
      {
        id: "b",
        found: true,
        manufacturer: "Oikade",
        model: "Simulated switch",
        capabilities: ["light.on"],
      },
      { id: "c", found: false, error: expect.stringContaining("fan.on") },
    ]);
  });
});

describe("apply", () => {
  it("switches and reports the new state", () => {
    const { switches } = plan(declared);
    expect(
      apply(switches, { id: "b", capability: "light.on", value: true })
    ).toEqual({
      state: { "light.on": true },
    });
    expect(switches.get("b")?.on).toBe(true);
  });

  it("rejects the wrong capability or value", () => {
    const { switches } = plan(declared);
    expect(
      apply(switches, { id: "a", capability: "light.on", value: true })
    ).toHaveProperty("error");
    expect(
      apply(switches, { id: "a", capability: "switch.on", value: 1 })
    ).toHaveProperty("error");
    expect(
      apply(switches, { id: "c", capability: "switch.on", value: true })
    ).toHaveProperty("error");
  });
});
