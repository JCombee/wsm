// The exec layer: one command in a sandbox through wsm's tenant API
// (tenant.ts). It keeps the interface of spike 05's podman layer, so the sync
// code works unchanged.
//
// - The command and its arguments travel as argv in the exec spec, the
//   environment as a map. Neither becomes argv of a process in the control
//   room.
// - The deadline runs in the sandbox, in the tenant API's wrapper. Dropping
//   the connection cancels, and the tenant API then ends the whole process
//   group, so no command outlives its exec.
// - stdout and stderr are capped by the tenant API and again here
//   (invariant 8).
import path from "node:path";
import { PassThrough, type Readable } from "node:stream";
import { printable, TenantClient, TenantError, type ExitInfo, type TenantExec, type TenantExecSpec } from "./tenant.ts";

export const DEFAULT_CWD = "/workspace";
export const DEFAULT_OUTPUT_LIMIT = 64 << 20;
export const MAX_OUTPUT_LIMIT = 512 << 20;
export const MAX_TIMEOUT_S = 4 * 60 * 60;
/** CLIENT_GRACE_MS lies past the tenant API's own guard (20 s after the deadline). */
export const CLIENT_GRACE_MS = 40_000;

const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

export class ExecError extends Error {}

export interface ExecTarget {
  sandbox: string;
}

export interface ExecSpec {
  target: ExecTarget;
  command: string;
  args?: readonly string[];
  cwd?: string;
  env?: Readonly<Record<string, string>>;
  interactive?: boolean;
  timeoutMs: number;
}

export interface ExecResult {
  exitCode: number | null;
  signal: string | null;
  timedOut: boolean;
  stopped: boolean;
  stdout: Buffer;
  stderr: Buffer;
  durationMs: number;
}

export interface ExecLog {
  info(msg: string): void;
  warn(msg: string): void;
}

export const silentLog: ExecLog = { info() {}, warn() {} };

let sharedClient: TenantClient | null = null;

/** tenantClient returns the client for the control room's tenant socket. */
export function tenantClient(): TenantClient {
  sharedClient ??= new TenantClient();
  return sharedClient;
}

/** setTenantClient replaces the shared client, for tests. */
export function setTenantClient(c: TenantClient): void {
  sharedClient = c;
}

function checkArg(what: string, v: unknown): string {
  if (typeof v !== "string") throw new ExecError(`${what} must be a string`);
  if (v.includes("\0")) throw new ExecError(`${what} contains a NUL byte`);
  return v;
}

/** toTenantSpec turns an exec spec into the tenant API's spec. */
export function toTenantSpec(spec: ExecSpec, outputLimit: number, interactive: boolean): TenantExecSpec {
  const command = checkArg("command", spec.command);
  if (command.length === 0 || command.startsWith("-")) throw new ExecError("invalid command");
  const args = (spec.args ?? []).map((a, i) => checkArg(`argument ${i}`, a));
  const cwd = checkArg("cwd", spec.cwd ?? DEFAULT_CWD);
  if (!path.posix.isAbsolute(cwd)) throw new ExecError("cwd must be an absolute path");
  if (!Number.isFinite(spec.timeoutMs) || spec.timeoutMs <= 0) throw new ExecError("timeout must be positive");
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(spec.env ?? {})) {
    if (!ENV_NAME_RE.test(name)) throw new ExecError("invalid environment variable name");
    if (typeof value !== "string" || value.includes("\0")) {
      throw new ExecError(`environment variable ${name} is not a string without NUL bytes`);
    }
    env[name] = value;
  }
  const limit = Math.min(Math.max(Math.floor(outputLimit), 1), MAX_OUTPUT_LIMIT);
  return {
    argv: [command, ...args],
    env,
    workdir: path.posix.resolve(cwd),
    timeoutSeconds: Math.min(MAX_TIMEOUT_S, Math.max(1, Math.ceil(spec.timeoutMs / 1000))),
    stdin: interactive,
    stdoutLimit: limit,
    stderrLimit: limit,
  };
}

/** describe is the loggable summary of an exec: never values, never stdin. */
export function describe(spec: ExecSpec): string {
  const names = Object.keys(spec.env ?? {}).sort();
  return `argv0=${JSON.stringify(spec.command.slice(0, 64))} args=${spec.args?.length ?? 0} env=[${names.join(",")}]`;
}

function toExecError(e: unknown): ExecError {
  if (e instanceof ExecError) return e;
  if (e instanceof TenantError) return new ExecError(e.message);
  return new ExecError(printable(e instanceof Error ? e.message : String(e)));
}

type StdinSource = string | Buffer | Readable | undefined;

async function feedStdin(ex: TenantExec, stdin: StdinSource, interactive: boolean): Promise<void> {
  if (!interactive) return;
  if (stdin === undefined) {
    await ex.endStdin();
    return;
  }
  if (typeof stdin === "string" || Buffer.isBuffer(stdin)) {
    await ex.writeStdin(typeof stdin === "string" ? Buffer.from(stdin) : stdin);
    await ex.endStdin();
    return;
  }
  try {
    for await (const chunk of stdin) await ex.writeStdin(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as Uint8Array));
    await ex.endStdin();
  } catch {
    // A broken source must not look like a complete input.
    ex.close();
  }
}

function toResult(info: ExitInfo, stdout: Buffer, stderr: Buffer, startedAt: number): ExecResult {
  return {
    exitCode: info.code,
    signal: info.stopped ? "SIGTERM" : null,
    timedOut: info.timedOut,
    stopped: info.stopped,
    stdout,
    stderr,
    durationMs: Date.now() - startedAt,
  };
}

export interface RunOptions {
  stdin?: StdinSource;
  outputLimit?: number;
  log?: ExecLog;
  client?: TenantClient;
}

/** runExec runs one command and collects its output within the limit. */
export async function runExec(spec: ExecSpec, opts: RunOptions = {}): Promise<ExecResult> {
  const limit = opts.outputLimit ?? DEFAULT_OUTPUT_LIMIT;
  const log = opts.log ?? silentLog;
  const interactive = spec.interactive ?? opts.stdin !== undefined;
  const tspec = toTenantSpec(spec, limit, interactive);
  const client = opts.client ?? tenantClient();
  const out: Buffer[] = [];
  const err: Buffer[] = [];
  let outSize = 0;
  let errSize = 0;
  let failure: ExecError | null = null;
  let ex: TenantExec | null = null;
  const startedAt = Date.now();
  try {
    ex = await client.openExec(spec.target.sandbox, tspec, {
      stdout(c) {
        outSize += c.length;
        if (outSize > limit) {
          failure ??= new ExecError(`stdout exceeds the limit of ${limit} bytes`);
          ex?.close();
          return;
        }
        out.push(c);
      },
      stderr(c) {
        errSize += c.length;
        if (errSize > limit) {
          failure ??= new ExecError(`stderr exceeds the limit of ${limit} bytes`);
          ex?.close();
          return;
        }
        err.push(c);
      },
      error(text) {
        failure ??= new ExecError(text);
      },
    });
  } catch (e) {
    throw toExecError(e);
  }
  const running = ex;
  let clientKilled = false;
  const guard = setTimeout(() => {
    clientKilled = true;
    log.warn(`s07 exec closed by the client: no exit ${CLIENT_GRACE_MS} ms after the deadline`);
    running.close();
  }, spec.timeoutMs + CLIENT_GRACE_MS);
  const feeding = feedStdin(running, opts.stdin, interactive);
  let info: ExitInfo;
  try {
    info = await running.done;
  } catch (e) {
    if (failure) throw failure;
    if (clientKilled) {
      return { exitCode: null, signal: null, timedOut: true, stopped: false, stdout: Buffer.concat(out), stderr: Buffer.concat(err), durationMs: Date.now() - startedAt };
    }
    throw toExecError(e);
  } finally {
    clearTimeout(guard);
    await feeding.catch(() => undefined);
  }
  if (failure) throw failure;
  if (info.reason === "limit") throw new ExecError(running.errors[0] ?? "output limit exceeded");
  return toResult(info, Buffer.concat(out), Buffer.concat(err), startedAt);
}

export interface StreamingExec {
  /** The command's stdout, for the caller to consume. */
  stdout: Readable;
  /** Resolves when the command ended, with the capped stderr. */
  done: Promise<ExecResult>;
  kill(): void;
}

/**
 * runStreaming runs a command whose stdout the caller reads as a stream (a tar
 * archive). The tenant API caps stdout at 512 MiB; the caller's reader applies
 * its own limits. stderr is capped here.
 */
export function runStreaming(spec: ExecSpec, opts: { stdin?: Readable | Buffer; stderrLimit?: number; log?: ExecLog; client?: TenantClient } = {}): StreamingExec {
  const log = opts.log ?? silentLog;
  const errLimit = opts.stderrLimit ?? 1 << 20;
  const interactive = opts.stdin !== undefined;
  const client = opts.client ?? tenantClient();
  const pass = new PassThrough();
  let ex: TenantExec | null = null;
  let killed = false;
  let paused = false;
  const startedAt = Date.now();
  const done = (async (): Promise<ExecResult> => {
    const err: Buffer[] = [];
    let errSize = 0;
    try {
      const tspec = toTenantSpec(spec, MAX_OUTPUT_LIMIT, interactive);
      ex = await client.openExec(spec.target.sandbox, tspec, {
        stdout(c) {
          // Backpressure: stop reading the socket until the consumer caught up.
          if (!pass.write(c) && !paused) {
            paused = true;
            ex?.pause();
            pass.once("drain", () => {
              paused = false;
              ex?.resume();
            });
          }
        },
        stderr(c) {
          errSize += c.length;
          if (errSize <= errLimit) err.push(c);
        },
      });
    } catch (e) {
      const x = toExecError(e);
      pass.destroy(x);
      throw x;
    }
    const running = ex;
    if (killed) running.close();
    let clientKilled = false;
    const guard = setTimeout(() => {
      clientKilled = true;
      log.warn(`s07 exec closed by the client: no exit ${CLIENT_GRACE_MS} ms after the deadline`);
      running.close();
    }, spec.timeoutMs + CLIENT_GRACE_MS);
    const feeding = feedStdin(running, opts.stdin, interactive);
    try {
      const info = await running.done;
      pass.end();
      if (info.reason === "limit") throw new ExecError(running.errors[0] ?? "output limit exceeded");
      return toResult(info, Buffer.alloc(0), Buffer.concat(err), startedAt);
    } catch (e) {
      const x = clientKilled ? new ExecError("exec timed out") : toExecError(e);
      pass.destroy(x);
      throw x;
    } finally {
      clearTimeout(guard);
      await feeding.catch(() => undefined);
    }
  })();
  done.catch(() => undefined);
  return {
    stdout: pass,
    done,
    kill() {
      killed = true;
      ex?.close();
    },
  };
}
