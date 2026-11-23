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
// Paperclip adds two envelope keys to every sandbox driver config. This
// provider only has ephemeral leases, so reuseLease must be false.
// archiveOnRelease is accepted as a boolean and ignored: Paperclip's
// environment test (the hello probe) always sets it to true, and this provider
// deletes the sandbox on release either way, which is the safe direction.

function describeKey(key: string): string {
  return JSON.stringify(key.length > 64 ? `${key.slice(0, 64)}…` : key);
}

export interface ParseOptions {
  /**
   * strict refuses unknown keys. It holds for the operator's saved config
   * (onEnvironmentValidateConfig). The config Paperclip hands to the lease and
   * exec hooks is built by the host and carries extra context keys (agentId,
   * for one), so those hooks parse leniently: the known keys are still
   * checked, other keys are ignored and reported in extraKeys.
   */
  strict?: boolean;
  extraKeys?: string[];
}

/** parseConfig validates a raw driver config. */
export function parseConfig(raw: unknown, opts: ParseOptions = { strict: true }): WsmConfig {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    throw new ConfigError("wsm config must be an object");
  }
  const obj = raw as Record<string, unknown>;
  for (const key of Object.keys(obj)) {
    if (key === "reuseLease") {
      if (obj[key] !== false) throw new ConfigError("wsm config.reuseLease is not supported; it must be false");
      continue;
    }
    if (key === "archiveOnRelease") {
      if (typeof obj[key] !== "boolean") throw new ConfigError("wsm config.archiveOnRelease must be a boolean");
      continue;
    }
    if (!KNOWN_KEYS.has(key)) {
      if (opts.strict !== false) throw new ConfigError(`wsm config has unknown key ${describeKey(key)}`);
      opts.extraKeys?.push(key.slice(0, 64));
    }
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
