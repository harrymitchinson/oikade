import { readFile } from "node:fs/promises";
import { join } from "node:path";

/** The contract version this SDK speaks. */
export const CONTRACT = 1;

/** Where the operator tells a plugin to connect and what to serve. */
export interface Environment {
  /** The broker, such as `mqtt://oikade-mosquitto.oikade-system.svc:1883`. */
  mqttUrl: string;
  /** The topic root, `oikade/v1/<namespace>/<plugin>`. */
  topic: string;
  /** The path of devices.json. */
  devicesFile: string;
  /** The mounted user Secret, when the Plugin sets `spec.secretRef`. */
  secretsDir?: string;
}

/** Reads the environment the operator sets for a plugin pod. */
export function readEnvironment(
  env: NodeJS.ProcessEnv = process.env
): Environment {
  const required = (name: string): string => {
    const value = env[name];
    if (value === undefined || value === "") {
      throw new Error(`${name} is not set; the Oikade operator sets it`);
    }
    return value;
  };
  const topic = required("OIKADE_TOPIC");
  if (!/^oikade\/v1\/[^/+#]+\/[^/+#]+$/.test(topic)) {
    throw new Error(
      `OIKADE_TOPIC must be oikade/v1/<namespace>/<plugin>, got ${topic}`
    );
  }
  const environment: Environment = {
    mqttUrl: required("OIKADE_MQTT_URL"),
    topic,
    devicesFile: required("OIKADE_DEVICES_FILE"),
  };
  const secretsDir = env.OIKADE_SECRETS_DIR;
  if (secretsDir !== undefined && secretsDir !== "") {
    environment.secretsDir = secretsDir;
  }
  return environment;
}

/** A Device declared for this plugin in Git. */
export interface DeclaredDevice {
  /** The plugin-local id from `Device.spec.plugin.id`. */
  id: string;
  /** The Device object's name. */
  name: string;
  /** The name shown in HomeKit and Grafana. */
  displayName: string;
  /** The Room object's name, when the Device has one. */
  room?: string;
  /** `Device.spec.plugin.parameters`, unvalidated. Validating them is the plugin's job. */
  parameters: Record<string, string>;
}

/** Reads and checks devices.json. */
export async function readDevices(file: string): Promise<DeclaredDevice[]> {
  const raw: unknown = JSON.parse(await readFile(file, "utf8"));
  if (!isRecord(raw)) {
    throw new Error(`${file}: expected a JSON object`);
  }
  if (raw.contract !== CONTRACT) {
    throw new Error(
      `${file}: contract ${String(raw.contract)} is not supported; this SDK speaks contract ${CONTRACT}`
    );
  }
  if (!Array.isArray(raw.devices)) {
    throw new Error(`${file}: devices must be a list`);
  }
  return raw.devices.map((d: unknown, i) =>
    parseDevice(d, `${file}: devices[${i}]`)
  );
}

function parseDevice(d: unknown, where: string): DeclaredDevice {
  if (!isRecord(d)) {
    throw new Error(`${where}: expected an object`);
  }
  const text = (key: string): string => {
    const value = d[key];
    if (typeof value !== "string" || value === "") {
      throw new Error(`${where}: ${key} must be a non-empty string`);
    }
    return value;
  };
  const parameters: Record<string, string> = {};
  if (d.parameters !== undefined) {
    if (!isRecord(d.parameters)) {
      throw new Error(`${where}: parameters must be an object`);
    }
    for (const [key, value] of Object.entries(d.parameters)) {
      if (typeof value !== "string") {
        throw new Error(`${where}: parameters.${key} must be a string`);
      }
      parameters[key] = value;
    }
  }
  const device: DeclaredDevice = {
    id: text("id"),
    name: text("name"),
    displayName: text("displayName"),
    parameters,
  };
  if (typeof d.room === "string" && d.room !== "") {
    device.room = d.room;
  }
  return device;
}

/**
 * Reads one key of the mounted user Secret, by convention the device id. Returns undefined when
 * the Plugin has no Secret or the key is missing. Never log the result.
 */
export async function readSecret(
  environment: Environment,
  key: string
): Promise<string | undefined> {
  if (environment.secretsDir === undefined || key.includes("/")) {
    return undefined;
  }
  try {
    return await readFile(join(environment.secretsDir, key), "utf8");
  } catch (error) {
    if (isRecord(error) && error.code === "ENOENT") {
      return undefined;
    }
    throw error;
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
