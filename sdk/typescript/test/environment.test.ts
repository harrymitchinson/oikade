import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  readDevices,
  readEnvironment,
  readSecret,
  relayCapability,
} from "../src/index.js";

const env = {
  OIKADE_MQTT_URL: "mqtt://broker:1883",
  OIKADE_TOPIC: "oikade/v1/home/sonoff",
  OIKADE_DEVICES_FILE: "/etc/oikade/config/devices.json",
};

async function tempDir() {
  return mkdtemp(join(tmpdir(), "oikade-sdk-"));
}

describe("readEnvironment", () => {
  it("reads the operator's variables", () => {
    expect(readEnvironment(env)).toEqual({
      mqttUrl: "mqtt://broker:1883",
      topic: "oikade/v1/home/sonoff",
      devicesFile: "/etc/oikade/config/devices.json",
    });
    expect(
      readEnvironment({ ...env, OIKADE_SECRETS_DIR: "/etc/oikade/secrets" })
        .secretsDir
    ).toBe("/etc/oikade/secrets");
  });

  it("names a missing variable", () => {
    expect(() => readEnvironment({ ...env, OIKADE_MQTT_URL: "" })).toThrow(
      /OIKADE_MQTT_URL/
    );
  });

  it("rejects a topic outside oikade/v1", () => {
    for (const topic of [
      "oikade/v2/home/x",
      "oikade/v1/home",
      "oikade/v1/home/x/y",
      "oikade/v1/+/x",
    ]) {
      expect(() => readEnvironment({ ...env, OIKADE_TOPIC: topic })).toThrow(
        /OIKADE_TOPIC/
      );
    }
  });
});

describe("readDevices", () => {
  it("reads the devices file", async () => {
    const file = join(await tempDir(), "devices.json");
    await writeFile(
      file,
      JSON.stringify({
        contract: 1,
        devices: [
          {
            id: "1000aabbcc",
            name: "porch-light",
            displayName: "Porch light",
            room: "porch",
            parameters: { capability: "light.on" },
          },
          { id: "2", name: "hall", displayName: "hall" },
        ],
      })
    );
    expect(await readDevices(file)).toEqual([
      {
        id: "1000aabbcc",
        name: "porch-light",
        displayName: "Porch light",
        room: "porch",
        parameters: { capability: "light.on" },
      },
      { id: "2", name: "hall", displayName: "hall", parameters: {} },
    ]);
  });

  it("rejects another contract or a bad device", async () => {
    const dir = await tempDir();
    const cases: Array<[unknown, RegExp]> = [
      [{ contract: 2, devices: [] }, /contract 2/],
      [{ contract: 1 }, /devices must be a list/],
      [
        { contract: 1, devices: [{ id: "x", name: "x" }] },
        /devices\[0\]: displayName/,
      ],
      [
        {
          contract: 1,
          devices: [
            { id: "x", name: "x", displayName: "x", parameters: { a: 1 } },
          ],
        },
        /parameters.a/,
      ],
    ];
    for (const [i, [content, error]] of cases.entries()) {
      const file = join(dir, `${i}.json`);
      await writeFile(file, JSON.stringify(content));
      await expect(readDevices(file)).rejects.toThrow(error);
    }
  });
});

describe("readSecret", () => {
  it("reads a key and treats a missing one as absent", async () => {
    const dir = await tempDir();
    await writeFile(join(dir, "1000aabbcc"), "lan-key");
    const environment = { ...readEnvironment(env), secretsDir: dir };
    expect(await readSecret(environment, "1000aabbcc")).toBe("lan-key");
    expect(await readSecret(environment, "missing")).toBeUndefined();
    expect(await readSecret(environment, "../1000aabbcc")).toBeUndefined();
    expect(
      await readSecret(readEnvironment(env), "1000aabbcc")
    ).toBeUndefined();
  });
});

describe("relayCapability", () => {
  it("defaults to switch.on and accepts lights and outlets", () => {
    expect(relayCapability({})).toEqual({ capability: "switch.on" });
    expect(relayCapability({ capability: "light.on" })).toEqual({
      capability: "light.on",
    });
    expect(relayCapability({ capability: "outlet.on" })).toEqual({
      capability: "outlet.on",
    });
  });

  it("rejects anything else", () => {
    expect(relayCapability({ capability: "fan.on" })).toEqual({
      error:
        "parameters.capability must be one of switch.on, light.on, outlet.on, got fan.on",
    });
  });
});
