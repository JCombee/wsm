// Duplex channels: Paperclip's callback bridge as one long-lived
// `podman --remote exec -i` per host route. The host's writes go to stdin,
// stdout goes back to the host as raw chunks, stderr goes only to the plugin
// log (bounded, sanitized) and never into the channel.
//
// - The host's command holds the bridge token. It never goes into argv: each
//   element travels as an environment value (S05_ARG_<i>, values in the podman
//   client's own environment, see exec.ts), and a small sh wrapper in the
//   sandbox rebuilds the argument vector and execs it. No log line holds it.
// - The wrapper writes its PID to a file in the sandbox. Stop and close signal
//   that PID with a second exec (TERM, KILL after 5 s), because killing the
//   podman client alone leaves the process in the container running.
// - The open params carry no driver config, so a channel opens only on a lease
//   this worker acquired or resolved before (fail closed).
// - stdout is untrusted sandbox output and is capped per channel (invariant 8).
import { createHash, randomBytes, randomUUID } from "node:crypto";
import path from "node:path";
import {
  CLIENT_GRACE_MS,
  runExec,
  startExec,
  type ExecLog,
  type ExecResult,
  type ExecSpec,
  type ExecTarget,
  type StartedExec,
} from "./exec.ts";

export const DUPLEX_TIMEOUT_MS = 4 * 60 * 60 * 1000;
export const DUPLEX_OUTPUT_LIMIT = 512 << 20;
export const STOP_GRACE_S = 5;
export const ARG_ENV_PREFIX = "S05_ARG_";
const MAX_COMMAND_ARGS = 64;
const MAX_COMMAND_BYTES = 256 << 10;
const STDERR_LOG_LIMIT = 4096;
const CLIENT_EXIT_WAIT_MS = 3000;
const MAX_RETIRED = 1024;

// $1 = PID file, $2 = argument count; the arguments come from S05_ARG_<i>.
// Every step is an exec, so the PID in the file stays the bridge process.
export const WRAPPER = [
  `printf '%s\\n' "$$" >"$1" || exit 125`,
  `n=$2`,
  `shift 2`,
  `i=0`,
  `while [ "$i" -lt "$n" ]; do`,
  `  eval "a=\\\${${ARG_ENV_PREFIX}$i}"`,
  `  unset "${ARG_ENV_PREFIX}$i"`,
  `  set -- "$@" "$a"`,
  `  i=$((i + 1))`,
  `done`,
  `unset a i n`,
  `exec "$@"`,
].join("\n");

// $1 = PID file, $2 = grace seconds. Absent or garbled PID files mean there is
// nothing left to stop.
export const STOP_SCRIPT = [
  `pid=$(cat -- "$1" 2>/dev/null) || exit 0`,
  `case $pid in ''|*[!0-9]*) exit 0 ;; esac`,
  `kill -TERM "$pid" 2>/dev/null || { rm -f -- "$1"; echo gone; exit 0; }`,
  `i=0`,
  `while [ "$i" -lt "$2" ]; do`,
  `  sleep 1`,
  `  kill -0 "$pid" 2>/dev/null || { rm -f -- "$1"; echo term; exit 0; }`,
  `  i=$((i + 1))`,
  `done`,
  `kill -KILL "$pid" 2>/dev/null`,
  `rm -f -- "$1"`,
  `echo kill`,
].join("\n");

export class DuplexError extends Error {}

/** DuplexSink is the host side: ctx.duplexChannel in the worker. */
export interface DuplexSink {
  data(hostRouteId: string, workerSessionId: string, chunk: Uint8Array): void;
  exit(hostRouteId: string, workerSessionId: string, exitCode: number | null, transportClosed?: boolean): void;
}

/** DuplexLease is what a channel needs from a resolved lease. */
export interface DuplexLease {
  leaseId: string;
  sandbox: string;
  target: ExecTarget;
}

export interface DuplexOpenRequest {
  hostRouteId: unknown;
  command: unknown;
}

export interface DuplexDeps {
  start(spec: ExecSpec): StartedExec;
  run(spec: ExecSpec): Promise<ExecResult>;
}

export interface DuplexOptions {
  sink: DuplexSink;
  log: ExecLog;
  cwd: string;
  deps?: DuplexDeps;
}

interface Channel {
  hostRouteId: string;
  workerSessionId: string;
  leaseId: string;
  sandbox: string;
  tag: string;
  target: ExecTarget;
  pidFile: string;
  exec: StartedExec;
  exited: boolean;
  exitedPromise: Promise<void>;
  terminating: Promise<void> | null;
  stdoutBytes: number;
  stderrBytes: number;
  stderrLogged: number;
  guard: NodeJS.Timeout;
}

interface Retired {
  workerSessionId: string;
  closed: Promise<void>;
}

/** routeTag is a short, loggable stand-in for an opaque host route ID. */
export function routeTag(hostRouteId: string): string {
  return createHash("sha256").update(hostRouteId).digest("hex").slice(0, 12);
}

/** sanitizeLogText makes untrusted bytes safe for one log line. */
export function sanitizeLogText(s: string): string {
  return s
    .replace(/[^\x20-\x7e]+/g, " ")
    .replace(/[A-Za-z0-9+/=_-]{32,}/g, "[redacted]")
    .trim();
}

/** checkCommand validates the host's argument vector. */
export function checkCommand(command: unknown): string[] {
  if (!Array.isArray(command) || command.length === 0 || command.length > MAX_COMMAND_ARGS) {
    throw new DuplexError("duplex command must be a non-empty argument vector");
  }
  let total = 0;
  const out: string[] = [];
  for (const v of command) {
    if (typeof v !== "string" || v.includes("\0")) throw new DuplexError("duplex command arguments must be strings without NUL bytes");
    total += Buffer.byteLength(v);
    out.push(v);
  }
  if (total > MAX_COMMAND_BYTES) throw new DuplexError("duplex command is too large");
  const argv0 = out[0] ?? "";
  if (argv0.length === 0 || argv0.startsWith("-")) throw new DuplexError("invalid duplex program");
  return out;
}

/** duplexSpec is the exec spec of a channel: the wrapper, the PID file, and the command as env values. */
export function duplexSpec(target: ExecTarget, command: readonly string[], pidFile: string, cwd: string): ExecSpec {
  const env: Record<string, string> = {};
  command.forEach((v, i) => {
    env[`${ARG_ENV_PREFIX}${i}`] = v;
  });
  return {
    target,
    command: "sh",
    args: ["-c", WRAPPER, "s05-duplex", pidFile, String(command.length)],
    cwd,
    env,
    interactive: true,
    timeoutMs: DUPLEX_TIMEOUT_MS,
  };
}

export class DuplexChannels {
  private sink: DuplexSink;
  private log: ExecLog;
  private cwd: string;
  private deps: DuplexDeps;
  private byRoute = new Map<string, Channel>();
  private bySession = new Map<string, Channel>();
  private retired = new Map<string, Retired>();

  constructor(opts: DuplexOptions) {
    this.sink = opts.sink;
    this.log = opts.log;
    this.cwd = opts.cwd;
    this.deps = opts.deps ?? { start: startExec, run: (spec) => runExec(spec) };
  }

  /** open starts the command and registers the channel under the host route ID. */
  open(req: DuplexOpenRequest, lease: DuplexLease): { hostRouteId: string; workerSessionId: string } {
    const hostRouteId = req.hostRouteId;
    if (typeof hostRouteId !== "string" || hostRouteId.length === 0 || hostRouteId.length > 256) {
      throw new DuplexError("invalid host route id");
    }
    if (this.byRoute.has(hostRouteId) || this.retired.has(hostRouteId)) {
      throw new DuplexError("host route has already been used");
    }
    const command = checkCommand(req.command);
    const pidFile = `/tmp/.s05-duplex-${randomBytes(8).toString("hex")}.pid`;
    const exec = this.deps.start(duplexSpec(lease.target, command, pidFile, this.cwd));
    let markExited = () => {};
    const exitedPromise = new Promise<void>((resolve) => {
      markExited = () => resolve();
    });
    const ch: Channel = {
      hostRouteId,
      workerSessionId: `duplex-${randomUUID()}`,
      leaseId: lease.leaseId,
      sandbox: lease.sandbox,
      tag: routeTag(hostRouteId),
      target: lease.target,
      pidFile,
      exec,
      exited: false,
      exitedPromise,
      terminating: null,
      stdoutBytes: 0,
      stderrBytes: 0,
      stderrLogged: 0,
      guard: setTimeout(() => {
        this.log.warn(`s05 duplex client killed sandbox=${ch.sandbox} route=${ch.tag}: no exit ${CLIENT_GRACE_MS} ms after the in-container timeout`);
        this.killClient(ch);
      }, DUPLEX_TIMEOUT_MS + CLIENT_GRACE_MS),
    };
    ch.guard.unref();
    this.byRoute.set(hostRouteId, ch);
    this.bySession.set(ch.workerSessionId, ch);

    const { child } = exec;
    // Data events come from I/O callbacks, so the first chunk reaches the host
    // after the open reply that binds the worker session ID.
    child.stdout?.on("data", (chunk: Buffer) => {
      ch.stdoutBytes += chunk.length;
      if (ch.stdoutBytes > DUPLEX_OUTPUT_LIMIT) {
        if (ch.stdoutBytes - chunk.length <= DUPLEX_OUTPUT_LIMIT) {
          this.log.warn(`s05 duplex output limit sandbox=${ch.sandbox} route=${ch.tag} limit=${DUPLEX_OUTPUT_LIMIT}`);
          void this.terminate(ch);
        }
        return;
      }
      this.sink.data(ch.hostRouteId, ch.workerSessionId, chunk);
    });
    child.stderr?.on("data", (chunk: Buffer) => {
      ch.stderrBytes += chunk.length;
      const room = STDERR_LOG_LIMIT - ch.stderrLogged;
      if (room <= 0) return;
      const piece = chunk.subarray(0, room);
      ch.stderrLogged += piece.length;
      const text = sanitizeLogText(piece.toString("utf8"));
      if (text.length > 0) this.log.info(`s05 duplex stderr sandbox=${ch.sandbox} route=${ch.tag} ${text}`);
    });
    const onExit = (code: number | null, signal: NodeJS.Signals | null) => {
      if (ch.exited) return;
      ch.exited = true;
      clearTimeout(ch.guard);
      const ms = Date.now() - exec.startedAt;
      this.log.info(
        `s05 duplex exit sandbox=${ch.sandbox} route=${ch.tag} code=${code ?? "none"} signal=${signal ?? "none"} stdout=${ch.stdoutBytes} stderr=${ch.stderrBytes} ms=${ms}`,
      );
      if (code === null) this.sink.exit(ch.hostRouteId, ch.workerSessionId, null, true);
      else this.sink.exit(ch.hostRouteId, ch.workerSessionId, code);
      markExited();
    };
    child.on("error", (e) => {
      this.log.warn(`s05 duplex client failed sandbox=${ch.sandbox} route=${ch.tag}: ${sanitizeLogText(e.message)}`);
      onExit(null, null);
    });
    child.on("close", onExit);

    const argv0 = path.posix.basename(command[0] ?? "").slice(0, 32);
    this.log.info(`s05 duplex open sandbox=${ch.sandbox} route=${ch.tag} argv0=${JSON.stringify(argv0)} args=${command.length - 1}`);
    return { hostRouteId, workerSessionId: ch.workerSessionId };
  }

  /** write passes host input to the channel; only the exact live pair applies. */
  async write(hostRouteId: string, workerSessionId: string, data: Uint8Array): Promise<void> {
    const ch = this.live(hostRouteId, workerSessionId);
    const stdin = ch?.exec.child.stdin;
    if (!ch || ch.exited || !stdin || stdin.destroyed || stdin.writableEnded || data.byteLength === 0) return;
    await new Promise<void>((resolve) => {
      stdin.write(data, () => resolve());
    });
  }

  /** stop ends the channel's process; only the exact live pair applies. */
  async stop(hostRouteId: string, workerSessionId: string): Promise<void> {
    const ch = this.live(hostRouteId, workerSessionId);
    if (!ch) return;
    this.log.info(`s05 duplex stop sandbox=${ch.sandbox} route=${ch.tag}`);
    await this.terminate(ch);
  }

  /** close is keyed by the host route ID and is idempotent. */
  async close(hostRouteId: string): Promise<{ hostRouteId: string; workerSessionId?: string }> {
    const ch = this.byRoute.get(hostRouteId);
    if (ch) {
      const closed = this.terminate(ch).catch(() => undefined);
      this.retire(ch, closed);
      await closed;
      this.log.info(`s05 duplex close sandbox=${ch.sandbox} route=${ch.tag}`);
      return { hostRouteId, workerSessionId: ch.workerSessionId };
    }
    const retired = this.retired.get(hostRouteId);
    if (retired) {
      await retired.closed;
      return { hostRouteId, workerSessionId: retired.workerSessionId };
    }
    return { hostRouteId };
  }

  /** closeLease drops the lease's channels before its sandbox is deleted. */
  closeLease(leaseId: string): void {
    for (const ch of [...this.byRoute.values()]) {
      if (ch.leaseId !== leaseId) continue;
      this.retire(ch, ch.exitedPromise);
      this.killClient(ch);
      this.log.info(`s05 duplex close sandbox=${ch.sandbox} route=${ch.tag} reason=lease-end`);
    }
  }

  /** closeAll closes every channel, for an orderly worker shutdown. */
  async closeAll(): Promise<void> {
    await Promise.allSettled([...this.byRoute.keys()].map((route) => this.close(route)));
  }

  get size(): number {
    return this.byRoute.size;
  }

  private live(hostRouteId: string, workerSessionId: string): Channel | null {
    const ch = this.bySession.get(workerSessionId);
    return ch && ch.hostRouteId === hostRouteId ? ch : null;
  }

  private retire(ch: Channel, closed: Promise<void>): void {
    this.byRoute.delete(ch.hostRouteId);
    this.bySession.delete(ch.workerSessionId);
    if (this.retired.size >= MAX_RETIRED) {
      const oldest = this.retired.keys().next().value;
      if (oldest !== undefined) this.retired.delete(oldest);
    }
    this.retired.set(ch.hostRouteId, { workerSessionId: ch.workerSessionId, closed });
  }

  private killClient(ch: Channel): void {
    const { child } = ch.exec;
    if (!ch.exited && child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  }

  // terminate stops the process in the container first, then the client.
  private terminate(ch: Channel): Promise<void> {
    ch.terminating ??= this.doTerminate(ch);
    return ch.terminating;
  }

  private async doTerminate(ch: Channel): Promise<void> {
    if (ch.exited) return;
    const t0 = Date.now();
    let result = "error";
    try {
      const r = await this.deps.run({
        target: ch.target,
        command: "sh",
        args: ["-c", STOP_SCRIPT, "s05-stop", ch.pidFile, String(STOP_GRACE_S)],
        cwd: "/",
        timeoutMs: (STOP_GRACE_S + 10) * 1000,
      });
      result = r.exitCode === 0 ? sanitizeLogText(r.stdout.toString("utf8")).slice(0, 16) || "none" : `exit-${r.exitCode ?? "none"}`;
    } catch (e) {
      result = `error ${sanitizeLogText(e instanceof Error ? e.message : String(e)).slice(0, 120)}`;
    }
    // The exec session ends with the process; give the client a moment to
    // report the real exit code before it is killed.
    await Promise.race([ch.exitedPromise, sleep(CLIENT_EXIT_WAIT_MS)]);
    const clientKilled = !ch.exited;
    this.killClient(ch);
    await Promise.race([ch.exitedPromise, sleep(CLIENT_EXIT_WAIT_MS)]);
    this.log.info(
      `s05 duplex terminate sandbox=${ch.sandbox} route=${ch.tag} in-container=${result} client-killed=${clientKilled} ms=${Date.now() - t0}`,
    );
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms).unref();
  });
}
