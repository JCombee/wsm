// Duplex channels: Paperclip's callback bridge as one long-lived tenant exec
// stream per host route (tenant.ts). The host's writes go out as stdin frames,
// stdout comes back to the host as raw chunks, and stderr goes only to the
// plugin log (bounded, sanitized), never into the channel.
//
// - The host's command holds the bridge token. It never goes into argv: each
//   element travels as an environment value (S07_ARG_<i>), and a small sh
//   wrapper in the sandbox rebuilds the argument vector and execs it.
// - Stop sends the stop frame. The tenant API then sends TERM to the
//   command's process group, KILL after 5 s, and answers with the exit frame.
//   Close stops first and then drops the connection; a dropped connection
//   alone also ends the process group (the tenant API's cancel).
// - The open params carry no driver config, so a channel opens only on a
//   lease this worker acquired or resolved before (fail closed).
// - stdout is untrusted sandbox output and is capped per channel (invariant 8).
import { createHash, randomUUID } from "node:crypto";
import path from "node:path";
import type { ExecLog } from "./exec.ts";
import type { ExecHandlers, ExitInfo, TenantExecSpec } from "./tenant.ts";

export const DUPLEX_TIMEOUT_S = 4 * 60 * 60;
export const DUPLEX_OUTPUT_LIMIT = 512 << 20;
export const ARG_ENV_PREFIX = "S07_ARG_";
const MAX_COMMAND_ARGS = 64;
const MAX_COMMAND_BYTES = 256 << 10;
const STDERR_LOG_LIMIT = 4096;
const STOP_WAIT_MS = 10_000;
const CLOSE_WAIT_MS = 3000;
const MAX_RETIRED = 1024;

// $1 = argument count; the arguments come from S07_ARG_<i>.
export const WRAPPER = [
  `n=$1`,
  `shift`,
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
}

export interface DuplexOpenRequest {
  hostRouteId: unknown;
  command: unknown;
}

/** DuplexExec is what a channel needs from an exec stream (TenantExec). */
export interface DuplexExec {
  readonly done: Promise<ExitInfo>;
  writeStdin(data: Uint8Array): Promise<void>;
  stop(): Promise<void>;
  close(): void;
}

export interface DuplexDeps {
  open(sandbox: string, spec: TenantExecSpec, handlers: ExecHandlers): Promise<DuplexExec>;
}

export interface DuplexOptions {
  sink: DuplexSink;
  log: ExecLog;
  cwd: string;
  deps: DuplexDeps;
}

interface Channel {
  hostRouteId: string;
  workerSessionId: string;
  leaseId: string;
  sandbox: string;
  tag: string;
  exec: DuplexExec;
  openedAt: number;
  exited: boolean;
  exitedPromise: Promise<void>;
  markExited: () => void;
  terminating: Promise<void> | null;
  stdoutBytes: number;
  stderrBytes: number;
  stderrLogged: number;
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

/** duplexSpec is the exec spec of a channel: the wrapper, with the command as env values. */
export function duplexSpec(command: readonly string[], cwd: string): TenantExecSpec {
  const env: Record<string, string> = {};
  command.forEach((v, i) => {
    env[`${ARG_ENV_PREFIX}${i}`] = v;
  });
  return {
    argv: ["sh", "-c", WRAPPER, "s07-duplex", String(command.length)],
    env,
    workdir: cwd,
    timeoutSeconds: DUPLEX_TIMEOUT_S,
    stdin: true,
    stdoutLimit: DUPLEX_OUTPUT_LIMIT,
    stderrLimit: DUPLEX_OUTPUT_LIMIT,
  };
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms).unref();
  });
}

export class DuplexChannels {
  private sink: DuplexSink;
  private log: ExecLog;
  private cwd: string;
  private deps: DuplexDeps;
  private byRoute = new Map<string, Channel>();
  private bySession = new Map<string, Channel>();
  private opening = new Set<string>();
  private retired = new Map<string, Retired>();

  constructor(opts: DuplexOptions) {
    this.sink = opts.sink;
    this.log = opts.log;
    this.cwd = opts.cwd;
    this.deps = opts.deps;
  }

  /** open starts the command and registers the channel under the host route ID. */
  async open(req: DuplexOpenRequest, lease: DuplexLease): Promise<{ hostRouteId: string; workerSessionId: string }> {
    const hostRouteId = req.hostRouteId;
    if (typeof hostRouteId !== "string" || hostRouteId.length === 0 || hostRouteId.length > 256) {
      throw new DuplexError("invalid host route id");
    }
    if (this.byRoute.has(hostRouteId) || this.retired.has(hostRouteId) || this.opening.has(hostRouteId)) {
      throw new DuplexError("host route has already been used");
    }
    const command = checkCommand(req.command);
    const workerSessionId = `duplex-${randomUUID()}`;
    const tag = routeTag(hostRouteId);
    let ch: Channel | null = null;
    const early: Array<() => void> = [];
    const handlers: ExecHandlers = {
      stdout: (chunk) => {
        if (ch) this.onStdout(ch, chunk);
        else early.push(() => ch && this.onStdout(ch, chunk));
      },
      stderr: (chunk) => {
        if (ch) this.onStderr(ch, chunk);
        else early.push(() => ch && this.onStderr(ch, chunk));
      },
    };
    this.opening.add(hostRouteId);
    let exec: DuplexExec;
    try {
      exec = await this.deps.open(lease.sandbox, duplexSpec(command, this.cwd), handlers);
    } finally {
      this.opening.delete(hostRouteId);
    }
    let markExited = () => {};
    const exitedPromise = new Promise<void>((resolve) => {
      markExited = () => resolve();
    });
    const opened: Channel = {
      hostRouteId,
      workerSessionId,
      leaseId: lease.leaseId,
      sandbox: lease.sandbox,
      tag,
      exec,
      openedAt: Date.now(),
      exited: false,
      exitedPromise,
      markExited,
      terminating: null,
      stdoutBytes: 0,
      stderrBytes: 0,
      stderrLogged: 0,
    };
    ch = opened;
    this.byRoute.set(hostRouteId, opened);
    this.bySession.set(workerSessionId, opened);
    exec.done.then(
      (info) => this.onExit(opened, info.code, info.reason || "exit"),
      () => this.onExit(opened, null, "transport"),
    );
    for (const f of early) f();
    const argv0 = path.posix.basename(command[0] ?? "").slice(0, 32);
    this.log.info(`s07 duplex open sandbox=${lease.sandbox} route=${tag} argv0=${JSON.stringify(argv0)} args=${command.length - 1}`);
    return { hostRouteId, workerSessionId };
  }

  private onStdout(ch: Channel, chunk: Buffer): void {
    ch.stdoutBytes += chunk.length;
    if (ch.stdoutBytes > DUPLEX_OUTPUT_LIMIT) {
      if (ch.stdoutBytes - chunk.length <= DUPLEX_OUTPUT_LIMIT) {
        this.log.warn(`s07 duplex output limit sandbox=${ch.sandbox} route=${ch.tag} limit=${DUPLEX_OUTPUT_LIMIT}`);
        void this.terminate(ch);
      }
      return;
    }
    this.sink.data(ch.hostRouteId, ch.workerSessionId, chunk);
  }

  private onStderr(ch: Channel, chunk: Buffer): void {
    ch.stderrBytes += chunk.length;
    const room = STDERR_LOG_LIMIT - ch.stderrLogged;
    if (room <= 0) return;
    const piece = chunk.subarray(0, room);
    ch.stderrLogged += piece.length;
    const text = sanitizeLogText(piece.toString("utf8"));
    if (text.length > 0) this.log.info(`s07 duplex stderr sandbox=${ch.sandbox} route=${ch.tag} ${text}`);
  }

  private onExit(ch: Channel, code: number | null, reason: string): void {
    if (ch.exited) return;
    ch.exited = true;
    this.log.info(
      `s07 duplex exit sandbox=${ch.sandbox} route=${ch.tag} code=${code ?? "none"} reason=${reason} stdout=${ch.stdoutBytes} stderr=${ch.stderrBytes} ms=${Date.now() - ch.openedAt}`,
    );
    if (code === null) this.sink.exit(ch.hostRouteId, ch.workerSessionId, null, true);
    else this.sink.exit(ch.hostRouteId, ch.workerSessionId, code);
    ch.markExited();
  }

  /** write passes host input to the channel; only the exact live pair applies. */
  async write(hostRouteId: string, workerSessionId: string, data: Uint8Array): Promise<void> {
    const ch = this.live(hostRouteId, workerSessionId);
    if (!ch || ch.exited || data.byteLength === 0) return;
    await ch.exec.writeStdin(data);
  }

  /** stop ends the channel's process; only the exact live pair applies. */
  async stop(hostRouteId: string, workerSessionId: string): Promise<void> {
    const ch = this.live(hostRouteId, workerSessionId);
    if (!ch) return;
    this.log.info(`s07 duplex stop sandbox=${ch.sandbox} route=${ch.tag}`);
    await this.terminate(ch);
  }

  /** close is keyed by the host route ID and is idempotent. */
  async close(hostRouteId: string): Promise<{ hostRouteId: string; workerSessionId?: string }> {
    const ch = this.byRoute.get(hostRouteId);
    if (ch) {
      const closed = this.terminate(ch)
        .catch(() => undefined)
        .then(() => ch.exec.close());
      this.retire(ch, closed);
      await closed;
      this.log.info(`s07 duplex close sandbox=${ch.sandbox} route=${ch.tag}`);
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
      ch.exec.close();
      this.log.info(`s07 duplex close sandbox=${ch.sandbox} route=${ch.tag} reason=lease-end`);
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

  // terminate asks the tenant API to stop the process group, and drops the
  // connection when no exit frame came in time.
  private terminate(ch: Channel): Promise<void> {
    ch.terminating ??= this.doTerminate(ch);
    return ch.terminating;
  }

  private async doTerminate(ch: Channel): Promise<void> {
    if (ch.exited) return;
    const t0 = Date.now();
    await ch.exec.stop().catch(() => undefined);
    const ended = await Promise.race([ch.exitedPromise.then(() => true), sleep(STOP_WAIT_MS).then(() => false)]);
    if (!ended) {
      ch.exec.close();
      await Promise.race([ch.exitedPromise, sleep(CLOSE_WAIT_MS)]);
    }
    this.log.info(`s07 duplex terminate sandbox=${ch.sandbox} route=${ch.tag} by=${ended ? "stop" : "cancel"} ms=${Date.now() - t0}`);
  }
}
