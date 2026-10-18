import { randomBytes } from "node:crypto";
import mqtt from "mqtt";

import { CONTRACT, type Environment, isRecord } from "./environment.js";
import { jsonLogger, type Logger } from "./log.js";

/** A capability value: JSON true or false, or a number within the capability's range. */
export type Value = boolean | number;

/** A device's full current values, keyed by canonical capability name. */
export type State = Record<string, Value>;

/** What the plugin knows about one declared device, as published on `devices`. */
export interface DeviceReport {
  id: string;
  /** True once the plugin can reach the device. */
  found: boolean;
  manufacturer?: string;
  model?: string;
  /** Canonical capability names from Oikade's registry. Only used when found. */
  capabilities?: string[];
  /** A configuration problem, shown on the Device as Resolved=False PluginError. */
  error?: string;
}

/** A command from Oikade: one capability of one device. */
export interface SetCommand {
  id: string;
  capability: string;
  value: Value;
}

/** The subset of an MQTT client the SDK uses. */
export interface Connection {
  publish(topic: string, payload: string, retain: boolean): Promise<void>;
  subscribe(filter: string): Promise<void>;
  onConnect(handler: () => void): void;
  onMessage(handler: (topic: string, payload: Buffer) => void): void;
  onError(handler: (error: Error) => void): void;
  /** Disconnects. force drops queued messages instead of waiting to send them. */
  end(force: boolean): Promise<void>;
}

/** The Last Will the broker publishes, retained on `status`, when it loses the plugin. */
export interface Will {
  topic: string;
  payload: string;
}

export type Connect = (url: string, will: Will) => Connection;

export interface PluginOptions {
  environment: Environment;
  /** Applies a command, then reports the result through setState or updateState. */
  onSet: (command: SetCommand, plugin: Plugin) => void | Promise<void>;
  log?: Logger;
  /** Replaces the MQTT client, for tests. */
  connect?: Connect;
  /**
   * How long start waits to come online before failing, so a wrong broker URL crashes the pod
   * instead of leaving it Running and silent. Defaults to 60 seconds.
   */
  startTimeoutMs?: number;
  /** How long close waits to publish the offline status. Defaults to 2 seconds. */
  closeTimeoutMs?: number;
}

const online = JSON.stringify({ state: "online", contract: CONTRACT });
const offline = JSON.stringify({ state: "offline", contract: CONTRACT });

/** Resolves true when promise settles within ms, false otherwise. */
async function within(promise: Promise<unknown>, ms: number): Promise<boolean> {
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<false>((resolve) => {
    timer = setTimeout(() => resolve(false), ms);
  });
  try {
    return await Promise.race([promise.then(() => true), timeout]);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * A running plugin. It keeps the latest devices, state and availability and republishes them,
 * retained, whenever it reconnects, because a Last Will or a broker restart may have replaced them.
 */
export class Plugin {
  private readonly root: string;
  private readonly connection: Connection;
  private readonly log: Logger;
  private readonly onSet: PluginOptions["onSet"];
  private devices: DeviceReport[] | undefined;
  private undeclared = 0;
  private readonly states = new Map<string, State>();
  private readonly availability = new Map<string, boolean>();
  private closed = false;
  private readonly closeTimeoutMs: number;

  private constructor(options: PluginOptions, connection: Connection) {
    this.root = options.environment.topic;
    this.connection = connection;
    this.log = options.log ?? jsonLogger;
    this.onSet = options.onSet;
    this.closeTimeoutMs = options.closeTimeoutMs ?? 2000;
  }

  /**
   * Connects with the Last Will set and resolves once the plugin is online. Rejects if it isn't
   * online within startTimeoutMs.
   */
  static async start(options: PluginOptions): Promise<Plugin> {
    const connect = options.connect ?? mqttConnect;
    const root = options.environment.topic;
    const connection = connect(options.environment.mqttUrl, {
      topic: `${root}/status`,
      payload: offline,
    });
    const plugin = new Plugin(options, connection);
    const started = new Promise<void>((resolve) => {
      let first = true;
      connection.onError((error) => {
        plugin.log.warn("MQTT connection failed", { error: error.message });
      });
      connection.onMessage((topic, payload) => {
        void plugin.handleMessage(topic, payload);
      });
      connection.onConnect(() => {
        plugin.announce().then(
          () => {
            plugin.log.info("Plugin came online", { topic: root });
            if (first) {
              first = false;
              resolve();
            }
          },
          (error: unknown) => {
            plugin.log.error("Could not announce plugin", {
              error: String(error),
            });
          }
        );
      });
    });
    const timeoutMs = options.startTimeoutMs ?? 60_000;
    if (!(await within(started, timeoutMs))) {
      plugin.closed = true;
      await connection.end(true);
      throw new Error(
        `could not come online at ${options.environment.mqttUrl} within ${timeoutMs}ms`
      );
    }
    return plugin;
  }

  /** Publishes the devices this plugin serves. List every device from devices.json. */
  async setDevices(devices: DeviceReport[], undeclared = 0): Promise<void> {
    this.devices = devices.map((d) => ({ ...d }));
    this.undeclared = undeclared;
    await this.publishDevices();
    // Drop values the devices no longer announce, and replace the retained state that holds them:
    // a hub that restarts would otherwise read and reject them.
    for (const [id, state] of this.states) {
      const announced = this.capabilities(id);
      const stale = Object.keys(state).filter((c) => !announced.includes(c));
      if (stale.length === 0) {
        continue;
      }
      for (const capability of stale) {
        delete state[capability];
      }
      await this.publishState(id);
    }
  }

  /** Publishes a device's full current values. Every capability must be announced in devices. */
  async setState(id: string, state: State): Promise<void> {
    const announced = this.capabilities(id);
    for (const capability of Object.keys(state)) {
      if (!announced.includes(capability)) {
        throw new Error(
          `device ${id} has not announced capability ${capability}; add it in setDevices first`
        );
      }
    }
    this.states.set(id, { ...state });
    await this.publishState(id);
  }

  /** Merges values into a device's state and publishes the full result. */
  async updateState(id: string, values: State): Promise<void> {
    await this.setState(id, { ...this.states.get(id), ...values });
  }

  /** Publishes whether a device is reachable. */
  async setAvailability(id: string, isOnline: boolean): Promise<void> {
    this.availability.set(id, isOnline);
    await this.publishAvailability(id);
  }

  /** The last state published for a device. */
  state(id: string): State | undefined {
    const state = this.states.get(id);
    return state === undefined ? undefined : { ...state };
  }

  /**
   * Marks the plugin offline and disconnects. A clean disconnect doesn't trigger the Last Will, so
   * the offline status is published first. If the broker can't take it within closeTimeoutMs, the
   * connection is dropped instead, which leaves the Last Will to the broker.
   */
  async close(): Promise<void> {
    if (this.closed) {
      return;
    }
    this.closed = true;
    const published = await within(
      this.connection.publish(`${this.root}/status`, offline, true),
      this.closeTimeoutMs
    );
    await this.connection.end(!published);
  }

  private async announce(): Promise<void> {
    await this.connection.subscribe(`${this.root}/+/set`);
    // close() may run while a reconnect announces; online must never land after its offline.
    if (this.closed) {
      return;
    }
    await this.connection.publish(`${this.root}/status`, online, true);
    // Until the plugin reports its devices, keep whatever is retained: an empty list would mark
    // every Device as not found while the plugin starts.
    if (this.devices !== undefined) {
      await this.publishDevices();
    }
    for (const id of this.availability.keys()) {
      await this.publishAvailability(id);
    }
    for (const id of this.states.keys()) {
      await this.publishState(id);
    }
  }

  private async publishDevices(): Promise<void> {
    const payload = {
      devices: this.devices ?? [],
      undeclared: this.undeclared,
    };
    await this.connection.publish(
      `${this.root}/devices`,
      JSON.stringify(payload),
      true
    );
  }

  private async publishState(id: string): Promise<void> {
    await this.connection.publish(
      `${this.root}/${id}/state`,
      JSON.stringify(this.states.get(id) ?? {}),
      true
    );
  }

  private async publishAvailability(id: string): Promise<void> {
    const value = this.availability.get(id) === true ? "online" : "offline";
    await this.connection.publish(
      `${this.root}/${id}/availability`,
      value,
      true
    );
  }

  private capabilities(id: string): string[] {
    const device = this.devices?.find((d) => d.id === id);
    if (device === undefined || !device.found) {
      return [];
    }
    return device.capabilities ?? [];
  }

  private async handleMessage(topic: string, payload: Buffer): Promise<void> {
    const parsed = parseSet(this.root, topic, payload);
    if (parsed === undefined) {
      return;
    }
    if ("error" in parsed) {
      this.log.warn("Ignored set command", { topic, reason: parsed.error });
      return;
    }
    if (!this.capabilities(parsed.id).includes(parsed.capability)) {
      this.log.warn("Ignored set command", {
        topic,
        reason: `device ${parsed.id} has not announced ${parsed.capability}`,
      });
      return;
    }
    try {
      await this.onSet(parsed, this);
    } catch (error) {
      this.log.error("Could not apply set command", {
        topic,
        capability: parsed.capability,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  }
}

/**
 * Parses a message on `<root>/<id>/set`. Returns undefined for other topics, and an error for a
 * payload that isn't exactly one capability with a boolean or number value.
 */
export function parseSet(
  root: string,
  topic: string,
  payload: Buffer
): SetCommand | { error: string } | undefined {
  const prefix = `${root}/`;
  if (!topic.startsWith(prefix) || !topic.endsWith("/set")) {
    return undefined;
  }
  const id = topic.slice(prefix.length, -"/set".length);
  if (id === "" || id.includes("/")) {
    return undefined;
  }
  let body: unknown;
  try {
    body = JSON.parse(payload.toString("utf8"));
  } catch {
    return { error: "payload is not JSON" };
  }
  if (!isRecord(body)) {
    return { error: "payload is not a JSON object" };
  }
  const entries = Object.entries(body);
  const [entry] = entries;
  if (entry === undefined || entries.length !== 1) {
    return { error: "payload must hold exactly one capability" };
  }
  const [capability, value] = entry;
  if (typeof value !== "boolean" && typeof value !== "number") {
    return { error: `${capability} must be a boolean or a number` };
  }
  return { id, capability, value };
}

function mqttConnect(url: string, will: Will): Connection {
  // A random client id: a takeover by a fixed id would fire the old session's Last Will after the
  // new session announced itself online.
  const client = mqtt.connect(url, {
    clientId: `oikade-plugin-${randomBytes(6).toString("hex")}`,
    clean: true,
    reconnectPeriod: 2000,
    will: {
      topic: will.topic,
      payload: Buffer.from(will.payload),
      qos: 1,
      retain: true,
    },
  });
  return {
    publish: async (topic, payload, retain) => {
      await client.publishAsync(topic, payload, { qos: 1, retain });
    },
    subscribe: async (filter) => {
      await client.subscribeAsync(filter, { qos: 1 });
    },
    onConnect: (handler) => {
      client.on("connect", handler);
    },
    onMessage: (handler) => {
      client.on("message", handler);
    },
    onError: (handler) => {
      client.on("error", handler);
    },
    end: (force) => client.endAsync(force),
  };
}
