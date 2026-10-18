import { describe, expect, it, vi } from "vitest";

import {
  type Connection,
  type Logger,
  parseSet,
  Plugin,
  type SetCommand,
  type Will,
} from "../src/index.js";

const root = "oikade/v1/home/example";

interface Published {
  topic: string;
  payload: string;
  retain: boolean;
}

class FakeConnection implements Connection {
  published: Published[] = [];
  subscribed: string[] = [];
  ended = false;
  forced = false;
  /** While set, publishes never complete, as when the broker is unreachable. */
  stalled = false;
  /** Runs inside subscribe, to interleave with an announce. */
  duringSubscribe?: () => void;
  private connect?: () => void;
  private message?: (topic: string, payload: Buffer) => void;

  constructor(readonly will: Will) {}

  async publish(topic: string, payload: string, retain: boolean) {
    if (this.stalled) {
      return new Promise<void>(() => {});
    }
    this.published.push({ topic, payload, retain });
  }
  async subscribe(filter: string) {
    this.subscribed.push(filter);
    this.duringSubscribe?.();
  }
  onConnect(handler: () => void) {
    this.connect = handler;
  }
  onMessage(handler: (topic: string, payload: Buffer) => void) {
    this.message = handler;
  }
  onError() {}
  async end(force: boolean) {
    this.ended = true;
    this.forced = force;
  }

  connected() {
    this.connect?.();
  }
  deliver(topic: string, payload: string) {
    this.message?.(topic, Buffer.from(payload));
  }
  last(topic: string): Published | undefined {
    return this.published.findLast((p) => p.topic === topic);
  }
}

const silent: Logger = { info() {}, warn() {}, error() {} };

async function start(onSet = vi.fn<(c: SetCommand) => void>()) {
  let connection: FakeConnection | undefined;
  const started = Plugin.start({
    environment: {
      mqttUrl: "mqtt://broker:1883",
      topic: root,
      devicesFile: "",
    },
    onSet,
    log: silent,
    connect: (_url, will) => {
      connection = new FakeConnection(will);
      return connection;
    },
  });
  connection!.connected();
  const plugin = await started;
  return { plugin, connection: connection!, onSet };
}

const relay = {
  id: "relay-1",
  found: true,
  manufacturer: "Oikade",
  model: "Simulated switch",
  capabilities: ["switch.on"],
};

describe("Plugin", () => {
  it("fails to start when it can't come online in time", async () => {
    let connection: FakeConnection | undefined;
    await expect(
      Plugin.start({
        environment: {
          mqttUrl: "mqtt://nowhere",
          topic: root,
          devicesFile: "",
        },
        onSet: () => {},
        log: silent,
        startTimeoutMs: 20,
        connect: (_url, will) => (connection = new FakeConnection(will)),
      })
    ).rejects.toThrow(/could not come online at mqtt:\/\/nowhere/);
    expect(connection?.forced).toBe(true);
  });

  it("closes without waiting forever for an unreachable broker", async () => {
    const { connection } = await start();
    const plugin = await Plugin.start({
      environment: { mqttUrl: "mqtt://x", topic: root, devicesFile: "" },
      onSet: () => {},
      log: silent,
      closeTimeoutMs: 20,
      connect: () => {
        setTimeout(() => connection.connected());
        return connection;
      },
    });
    connection.stalled = true;
    await plugin.close();
    expect(connection.forced).toBe(true);
  });

  it("never announces online after closing", async () => {
    const { plugin, connection } = await start();
    connection.duringSubscribe = () => void plugin.close();
    connection.connected();
    await vi.waitFor(() => expect(connection.ended).toBe(true));
    expect(connection.last(`${root}/status`)?.payload).toBe(
      '{"state":"offline","contract":1}'
    );
  });

  it("replaces the retained state when a capability is no longer announced", async () => {
    const { plugin, connection } = await start();
    await plugin.setDevices([
      { ...relay, capabilities: ["light.on", "light.level"] },
    ]);
    await plugin.setState("relay-1", { "light.on": true, "light.level": 40 });
    await plugin.setDevices([{ ...relay, capabilities: ["light.on"] }]);
    expect(connection.last(`${root}/relay-1/state`)?.payload).toBe(
      '{"light.on":true}'
    );
    await plugin.setDevices([{ id: "relay-1", found: false }]);
    expect(connection.last(`${root}/relay-1/state`)?.payload).toBe("{}");
  });

  it("sets a retained offline Last Will and announces itself online", async () => {
    const { connection } = await start();
    expect(connection.will).toEqual({
      topic: `${root}/status`,
      payload: '{"state":"offline","contract":1}',
    });
    expect(connection.subscribed).toEqual([`${root}/+/set`]);
    expect(connection.last(`${root}/status`)).toEqual({
      topic: `${root}/status`,
      payload: '{"state":"online","contract":1}',
      retain: true,
    });
  });

  it("leaves the retained devices alone until the plugin reports them", async () => {
    const { plugin, connection } = await start();
    connection.connected();
    await vi.waitFor(() =>
      expect(
        connection.published.filter((p) => p.topic === `${root}/status`)
      ).toHaveLength(2)
    );
    expect(connection.last(`${root}/devices`)).toBeUndefined();
    await plugin.setDevices([]);
    expect(connection.last(`${root}/devices`)?.payload).toBe(
      '{"devices":[],"undeclared":0}'
    );
  });

  it("publishes devices, state and availability retained", async () => {
    const { plugin, connection } = await start();
    await plugin.setDevices(
      [relay, { id: "bad", found: false, error: "nope" }],
      2
    );
    await plugin.setAvailability("relay-1", true);
    await plugin.setState("relay-1", { "switch.on": true });

    expect(JSON.parse(connection.last(`${root}/devices`)!.payload)).toEqual({
      devices: [relay, { id: "bad", found: false, error: "nope" }],
      undeclared: 2,
    });
    expect(connection.last(`${root}/relay-1/availability`)?.payload).toBe(
      "online"
    );
    expect(connection.last(`${root}/relay-1/state`)?.payload).toBe(
      '{"switch.on":true}'
    );
    expect(connection.published.every((p) => p.retain)).toBe(true);
  });

  it("refuses state for capabilities a device hasn't announced", async () => {
    const { plugin } = await start();
    await plugin.setDevices([relay]);
    await expect(
      plugin.setState("relay-1", { "light.on": true })
    ).rejects.toThrow(/not announced capability light.on/);
    await expect(
      plugin.setState("unknown", { "switch.on": true })
    ).rejects.toThrow(/not announced/);
  });

  it("merges updates into the full state", async () => {
    const { plugin, connection } = await start();
    await plugin.setDevices([
      { ...relay, capabilities: ["light.on", "light.level"] },
    ]);
    await plugin.setState("relay-1", { "light.on": true, "light.level": 40 });
    await plugin.updateState("relay-1", { "light.level": 80 });
    expect(
      JSON.parse(connection.last(`${root}/relay-1/state`)!.payload)
    ).toEqual({
      "light.on": true,
      "light.level": 80,
    });
  });

  it("dispatches /set only for announced capabilities", async () => {
    const { plugin, connection, onSet } = await start();
    await plugin.setDevices([relay]);
    connection.deliver(`${root}/relay-1/set`, '{"switch.on":false}');
    connection.deliver(`${root}/relay-1/set`, '{"light.on":false}');
    connection.deliver(`${root}/other/set`, '{"switch.on":false}');
    connection.deliver(`${root}/relay-1/set`, "not json");
    await vi.waitFor(() => expect(onSet).toHaveBeenCalledTimes(1));
    expect(onSet).toHaveBeenCalledWith(
      { id: "relay-1", capability: "switch.on", value: false },
      plugin
    );
  });

  it("survives a failing set handler", async () => {
    const error = vi.fn();
    let connection: FakeConnection | undefined;
    const started = Plugin.start({
      environment: { mqttUrl: "mqtt://x", topic: root, devicesFile: "" },
      onSet: () => {
        throw new Error("relay unreachable");
      },
      log: { ...silent, error },
      connect: (_url, will) => (connection = new FakeConnection(will)),
    });
    connection!.connected();
    const plugin = await started;
    await plugin.setDevices([relay]);
    connection!.deliver(`${root}/relay-1/set`, '{"switch.on":true}');
    await vi.waitFor(() =>
      expect(error).toHaveBeenCalledWith(
        "Could not apply set command",
        expect.objectContaining({ error: "relay unreachable" })
      )
    );
  });

  it("republishes everything after reconnecting", async () => {
    const { plugin, connection } = await start();
    await plugin.setDevices([relay]);
    await plugin.setAvailability("relay-1", true);
    await plugin.setState("relay-1", { "switch.on": true });
    connection.published = [];

    connection.connected();
    await vi.waitFor(() =>
      expect(connection.published.map((p) => p.topic)).toEqual([
        `${root}/status`,
        `${root}/devices`,
        `${root}/relay-1/availability`,
        `${root}/relay-1/state`,
      ])
    );
    expect(connection.subscribed).toEqual([`${root}/+/set`, `${root}/+/set`]);
  });

  it("publishes offline before disconnecting", async () => {
    const { plugin, connection } = await start();
    await plugin.close();
    expect(connection.last(`${root}/status`)).toEqual({
      topic: `${root}/status`,
      payload: '{"state":"offline","contract":1}',
      retain: true,
    });
    expect(connection.ended).toBe(true);
  });
});

describe("parseSet", () => {
  const set = (topic: string, payload: string) =>
    parseSet(root, topic, Buffer.from(payload));

  it("parses one capability", () => {
    expect(set(`${root}/a.b-c_1/set`, '{"light.level":30}')).toEqual({
      id: "a.b-c_1",
      capability: "light.level",
      value: 30,
    });
  });

  it("ignores other topics", () => {
    expect(set(`${root}/a/state`, "{}")).toBeUndefined();
    expect(set(`${root}/set`, "{}")).toBeUndefined();
    expect(set(`oikade/v1/home/other/a/set`, "{}")).toBeUndefined();
  });

  it("rejects payloads that aren't one boolean or number", () => {
    for (const payload of [
      "x",
      "[]",
      "{}",
      '{"a":1,"b":2}',
      '{"a":"on"}',
      '{"a":null}',
    ]) {
      expect(set(`${root}/a/set`, payload)).toHaveProperty("error");
    }
  });
});
