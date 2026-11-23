// Driver config for the wsm sandbox provider. The config names a wsm project
// and an optional default command timeout. It never holds secrets: the plugin
// reaches wsm through fixed local sockets (see admin.ts).

/** A wsm project or sandbox slug, as in internal/sandbox (slugPattern). */
export const SLUG_RE = /^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$/;

export const DEFAULT_TIMEOUT_MS = 900_000;
export const MAX_TIMEOUT_MS = 6 * 60 * 60 * 1000;

export interface WsmConfig {
  project: string;
  timeoutMs: number;
}

export class ConfigError extends Error {}

const KNOWN_KEYS = new Set(["project", "timeoutMs"]);

function describeKey(key: string): string {
  return JSON.stringify(key.length > 64 ? `${key.slice(0, 64)}…` : key);
}

/** parseConfig validates a raw driver config strictly. */
export function parseConfig(raw: unknown): WsmConfig {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    throw new ConfigError("wsm config must be an object");
  }
  const obj = raw as Record<string, unknown>;
  for (const key of Object.keys(obj)) {
    if (!KNOWN_KEYS.has(key)) throw new ConfigError(`wsm config has unknown key ${describeKey(key)}`);
  }
  const project = obj.project;
  if (typeof project !== "string" || !SLUG_RE.test(project)) {
    throw new ConfigError("wsm config.project must be a wsm project slug");
  }
  let timeoutMs = DEFAULT_TIMEOUT_MS;
  if (obj.timeoutMs !== undefined) {
    const t = obj.timeoutMs;
    if (typeof t !== "number" || !Number.isSafeInteger(t) || t < 1000 || t > MAX_TIMEOUT_MS) {
      throw new ConfigError(`wsm config.timeoutMs must be an integer between 1000 and ${MAX_TIMEOUT_MS}`);
    }
    timeoutMs = t;
  }
  return { project, timeoutMs };
}
