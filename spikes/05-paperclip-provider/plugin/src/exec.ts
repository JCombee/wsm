// The exec layer: runs one command in a sandbox's work container through the
// project's Podman socket, with the podman CLI in remote mode.
//
// - The podman client is spawned directly (absolute path, no shell).
// - Environment values never go into argv: each name goes as `-e NAME`, and the
//   value sits only in the podman client's own environment. Names that podman
//   itself reads are the exception; they go as `-e NAME=VALUE`, so they cannot
//   steer the client.
// - The client's environment is otherwise minimal, never the worker's own.
// - The command runs under `timeout` inside the container, because killing the
//   podman client leaves the process in the container running.
// - stdout and stderr are capped (invariant 8).
import { spawn, type ChildProcess } from "node:child_process";
import os from "node:os";
import path from "node:path";

export const PODMAN = "/usr/bin/podman";
export const CONTAINER_USER = "1000:1000";
export const DEFAULT_CWD = "/workspace";
export const DEFAULT_OUTPUT_LIMIT = 64 << 20;
export const CLIENT_GRACE_MS = 20_000;
const KILL_AFTER = "10s";

const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;
// Names the podman client (or its Go runtime) reads from its own environment.
// The brief's list, plus GO*, NOTIFY_SOCKET, REGISTRY_* and STORAGE_* as a
// conservative extension.
const PODMAN_READ_RE =
  /^(CONTAINER.*|PODMAN_.*|DOCKER_.*|BUILDAH_.*|XDG_.*|LD_.*|SSL_.*|GO.*|REGISTRY_.*|STORAGE_.*|NOTIFY_SOCKET|HOME|PATH|TMPDIR|HTTP_PROXY|HTTPS_PROXY|FTP_PROXY|NO_PROXY|ALL_PROXY)$/i;

export class ExecError extends Error {}

export interface ExecTarget {
  socket: string;
  container: string;
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

export interface PodmanInvocation {
  argv: string[];
  env: Record<string, string>;
  envNames: string[];
}

export function isPodmanReadName(name: string): boolean {
  return PODMAN_READ_RE.test(name);
}

function checkArg(what: string, v: unknown): string {
  if (typeof v !== "string") throw new ExecError(`${what} must be a string`);
  if (v.includes("\0")) throw new ExecError(`${what} contains a NUL byte`);
  return v;
}

/** buildInvocation turns an exec spec into podman argv plus client env. */
export function buildInvocation(spec: ExecSpec): PodmanInvocation {
  const { socket, container } = spec.target;
  if (!/^\/run\/wsm-podman\/wsm-p-[0-9]+\/podman\.sock$/.test(socket)) throw new ExecError("unexpected podman socket");
  if (!/^sbx-[a-z0-9]{12}-work$/.test(container)) throw new ExecError("unexpected container name");
  const command = checkArg("command", spec.command);
  if (command.length === 0 || command.startsWith("-")) throw new ExecError("invalid command");
  const args = (spec.args ?? []).map((a, i) => checkArg(`argument ${i}`, a));
  const cwd = checkArg("cwd", spec.cwd ?? DEFAULT_CWD);
  if (!path.posix.isAbsolute(cwd)) throw new ExecError("cwd must be an absolute path");
  if (!Number.isFinite(spec.timeoutMs) || spec.timeoutMs <= 0) throw new ExecError("timeout must be positive");

  const env: Record<string, string> = { PATH: "/usr/bin:/bin", HOME: os.userInfo().homedir, LANG: "C.UTF-8" };
  const envArgs: string[] = [];
  const envNames: string[] = [];
  for (const [name, value] of Object.entries(spec.env ?? {})) {
    if (!ENV_NAME_RE.test(name)) throw new ExecError("invalid environment variable name");
    if (typeof value !== "string" || value.includes("\0")) {
      throw new ExecError(`environment variable ${name} is not a string without NUL bytes`);
    }
    envNames.push(name);
    if (isPodmanReadName(name)) {
      if (/[\r\n@]/.test(value)) {
        throw new ExecError(`environment variable ${name} is read by podman and may not hold a newline or @`);
      }
      envArgs.push("-e", `${name}=${value}`);
    } else {
      envArgs.push("-e", name);
      env[name] = value;
    }
  }
  const seconds = Math.max(1, Math.ceil(spec.timeoutMs / 1000));
  const argv = [
    "--remote", "--url", `unix://${socket}`,
    "exec", ...(spec.interactive ? ["-i"] : []),
    "--user", CONTAINER_USER, "--workdir", cwd,
    ...envArgs,
    container,
    "timeout", "-k", KILL_AFTER, `${seconds}s`,
    command, ...args,
  ];
  return { argv, env, envNames: envNames.sort() };
}

export interface ExecResult {
  exitCode: number | null;
  signal: string | null;
  timedOut: boolean;
  stdout: Buffer;
  stderr: Buffer;
  durationMs: number;
}

export interface ExecLog {
  info(msg: string): void;
  warn(msg: string): void;
}

export const silentLog: ExecLog = { info() {}, warn() {} };

export interface StartedExec {
  child: ChildProcess;
  invocation: PodmanInvocation;
  startedAt: number;
}

/** startExec spawns the podman client. The caller owns the child's streams. */
export function startExec(spec: ExecSpec): StartedExec {
  const invocation = buildInvocation(spec);
  const child = spawn(PODMAN, invocation.argv, {
    env: invocation.env,
    cwd: "/",
    shell: false,
    stdio: [spec.interactive ? "pipe" : "ignore", "pipe", "pipe"],
  });
  child.stdin?.on("error", () => {});
  return { child, invocation, startedAt: Date.now() };
}

/** describe is the loggable summary of an exec: never values, never stdin. */
export function describe(spec: ExecSpec): string {
  const names = Object.keys(spec.env ?? {}).sort();
  return `argv0=${JSON.stringify(spec.command.slice(0, 64))} args=${spec.args?.length ?? 0} env=[${names.join(",")}]`;
}

export interface RunOptions {
  stdin?: string | Buffer;
  outputLimit?: number;
  log?: ExecLog;
}

/** runExec runs one command and collects its output within the limit. */
export function runExec(spec: ExecSpec, opts: RunOptions = {}): Promise<ExecResult> {
  const limit = opts.outputLimit ?? DEFAULT_OUTPUT_LIMIT;
  const log = opts.log ?? silentLog;
  const interactive = spec.interactive ?? opts.stdin !== undefined;
  const { child, startedAt } = startExec({ ...spec, interactive });
  return new Promise((resolve, reject) => {
    const out: Buffer[] = [];
    const err: Buffer[] = [];
    let outSize = 0;
    let errSize = 0;
    let failure: Error | null = null;
    let clientKilled = false;
    const kill = () => {
      if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    };
    const guard = setTimeout(() => {
      clientKilled = true;
      log.warn(`s05 exec client killed: no exit ${CLIENT_GRACE_MS} ms after the in-container timeout`);
      kill();
    }, spec.timeoutMs + CLIENT_GRACE_MS);
    child.stdout?.on("data", (c: Buffer) => {
      outSize += c.length;
      if (outSize > limit) {
        failure ??= new ExecError(`stdout exceeds the limit of ${limit} bytes`);
        kill();
        return;
      }
      out.push(c);
    });
    child.stderr?.on("data", (c: Buffer) => {
      errSize += c.length;
      if (errSize > limit) {
        failure ??= new ExecError(`stderr exceeds the limit of ${limit} bytes`);
        kill();
        return;
      }
      err.push(c);
    });
    child.on("error", (e) => {
      clearTimeout(guard);
      reject(new ExecError(`podman client failed to start: ${e.message}`));
    });
    child.on("close", (code, signal) => {
      clearTimeout(guard);
      const durationMs = Date.now() - startedAt;
      if (failure) {
        reject(failure);
        return;
      }
      const inContainerTimeout = (code === 124 || code === 137) && durationMs >= spec.timeoutMs - 500;
      resolve({
        exitCode: clientKilled ? null : code,
        signal: signal ?? null,
        timedOut: clientKilled || inContainerTimeout,
        stdout: Buffer.concat(out),
        stderr: Buffer.concat(err),
        durationMs,
      });
    });
    if (child.stdin) child.stdin.end(opts.stdin ?? "");
  });
}
