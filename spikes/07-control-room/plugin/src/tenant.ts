// Client for wsm's tenant API on the control room's only socket,
// /run/wsm/tenant.sock. The project is the socket's: no call names one.
//
// - JSON routes: whoami, list, create, get, start, stop and delete.
// - Exec: POST /v1/tenant/sandboxes/{id}/exec with Connection: Upgrade and
//   Upgrade: wsm-exec.v1, and the spec as the JSON body. A refusal is an
//   ordinary HTTP error. After the 101 both sides speak frames: one type byte,
//   a big-endian uint32 length and at most 1 MiB of payload. Dropping the
//   connection cancels the command.
// - Everything that comes back is untrusted: sizes are capped and JSON is
//   checked before use (invariant 8).
import http from "node:http";
import type { Socket } from "node:net";
import { SANDBOX_ID_RE } from "./lease.ts";

export const TENANT_SOCKET = "/run/wsm/tenant.sock";
export const EXEC_PROTOCOL = "wsm-exec.v1";
export const MAX_FRAME = 1 << 20;
export const FRAME = {
  stdin: 0x00,
  stdinEOF: 0x01,
  stop: 0x02,
  stdout: 0x10,
  stderr: 0x11,
  exit: 0x12,
  error: 0x13,
} as const;

// Create answers only once the sandbox runs a command, within 90 s.
const REQUEST_TIMEOUT_MS = 150_000;
const MAX_RESPONSE = 1 << 20;
const MAX_ERROR_TEXT = 300;
const STATUS_RE = /^[a-z_]{1,32}$/;
const PROJECT_RE = /^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$/;

export class TenantError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

export interface TenantSandbox {
  id: string;
  status: string;
}

export interface WhoAmI {
  project: string;
  peerUid: number;
}

export interface TenantExecSpec {
  argv: string[];
  env?: Record<string, string>;
  workdir?: string;
  timeoutSeconds: number;
  stdin: boolean;
  stdoutLimit?: number;
  stderrLimit?: number;
}

export interface ExitInfo {
  code: number | null;
  timedOut: boolean;
  stopped: boolean;
  reason: string;
}

export interface ExecHandlers {
  stdout(chunk: Buffer): void;
  stderr(chunk: Buffer): void;
  /** error receives the tenant API's error frames: a limit or protocol error. */
  error?(text: string): void;
}

interface Reply {
  status: number;
  body: unknown;
}

function isObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

/** printable makes untrusted text safe for an error message or a log line. */
export function printable(s: string, max = MAX_ERROR_TEXT): string {
  return s.replace(/[^\x20-\x7e]+/g, " ").trim().slice(0, max);
}

function errorText(body: unknown): string {
  return isObject(body) && typeof body.error === "string" ? printable(body.error) : "";
}

export function parseSandbox(v: unknown): TenantSandbox {
  if (!isObject(v) || typeof v.id !== "string" || !SANDBOX_ID_RE.test(v.id) || typeof v.status !== "string" || !STATUS_RE.test(v.status)) {
    throw new TenantError("tenant API returned a malformed sandbox", 0);
  }
  return { id: v.id, status: v.status };
}

/** parseExit checks the payload of an exit frame. */
export function parseExit(payload: Buffer): ExitInfo {
  let v: unknown;
  try {
    v = JSON.parse(payload.toString("utf8"));
  } catch {
    throw new TenantError("exit frame is not JSON", 0);
  }
  if (!isObject(v)) throw new TenantError("exit frame is not an object", 0);
  const code = v.code;
  if (!(code === null || code === undefined || (typeof code === "number" && Number.isInteger(code) && code >= -1 && code <= 255))) {
    throw new TenantError("exit frame has an invalid code", 0);
  }
  const reason = typeof v.reason === "string" ? v.reason.replace(/[^a-z_-]/g, "").slice(0, 32) : "";
  return { code: typeof code === "number" ? code : null, timedOut: v.timed_out === true, stopped: v.stopped === true, reason };
}

/** encodeFrame builds one frame. */
export function encodeFrame(type: number, payload: Uint8Array = new Uint8Array(0)): Buffer {
  if (payload.byteLength > MAX_FRAME) throw new TenantError("frame payload too large", 0);
  const b = Buffer.allocUnsafe(5 + payload.byteLength);
  b[0] = type;
  b.writeUInt32BE(payload.byteLength, 1);
  Buffer.from(payload.buffer, payload.byteOffset, payload.byteLength).copy(b, 5);
  return b;
}

export interface Frame {
  type: number;
  payload: Buffer;
}

/** FrameDecoder splits a byte stream into frames; partial frames wait for more data. */
export class FrameDecoder {
  private buf: Buffer = Buffer.alloc(0);

  push(chunk: Buffer): Frame[] {
    this.buf = this.buf.length === 0 ? chunk : Buffer.concat([this.buf, chunk]);
    const out: Frame[] = [];
    while (this.buf.length >= 5) {
      const type = this.buf[0] ?? 0;
      const len = this.buf.readUInt32BE(1);
      if (len > MAX_FRAME) throw new TenantError(`frame of ${len} bytes is above the limit`, 0);
      if (this.buf.length < 5 + len) break;
      out.push({ type, payload: this.buf.subarray(5, 5 + len) });
      this.buf = this.buf.subarray(5 + len);
    }
    return out;
  }

  get pending(): number {
    return this.buf.length;
  }
}

/** TenantExec is one exec stream after the upgrade. */
export class TenantExec {
  readonly done: Promise<ExitInfo>;
  /** errors holds the text of the error frames, in order. */
  readonly errors: string[] = [];
  private socket: Socket;
  private handlers: ExecHandlers;
  private decoder = new FrameDecoder();
  private settled = false;
  private resolveDone: (e: ExitInfo) => void = () => {};
  private rejectDone: (e: Error) => void = () => {};

  constructor(socket: Socket, head: Buffer, handlers: ExecHandlers) {
    this.socket = socket;
    this.handlers = handlers;
    this.done = new Promise<ExitInfo>((resolve, reject) => {
      this.resolveDone = resolve;
      this.rejectDone = reject;
    });
    this.done.catch(() => {});
    socket.setTimeout(0);
    socket.on("error", (e) => this.fail(new TenantError(`exec stream: ${printable(e.message)}`, 0)));
    socket.on("close", () => this.fail(new TenantError("exec stream ended without an exit frame", 0)));
    // Frames that came with the 101 are handled once the caller has the
    // stream. Later data waits in the socket until then: a data listener
    // added now would deliver it before the head, out of order.
    setImmediate(() => {
      if (head.length > 0) this.onData(head);
      socket.on("data", (c: Buffer) => this.onData(c));
    });
  }

  private onData(chunk: Buffer): void {
    if (this.settled) return;
    let frames: Frame[];
    try {
      frames = this.decoder.push(chunk);
    } catch (e) {
      this.fail(e instanceof Error ? e : new TenantError(String(e), 0));
      this.socket.destroy();
      return;
    }
    for (const f of frames) {
      switch (f.type) {
        case FRAME.stdout:
          this.handlers.stdout(f.payload);
          break;
        case FRAME.stderr:
          this.handlers.stderr(f.payload);
          break;
        case FRAME.error: {
          const text = printable(f.payload.toString("utf8"));
          this.errors.push(text);
          this.handlers.error?.(text);
          break;
        }
        case FRAME.exit:
          try {
            const info = parseExit(f.payload);
            this.settled = true;
            this.resolveDone(info);
          } catch (e) {
            this.fail(e instanceof Error ? e : new TenantError(String(e), 0));
          }
          this.socket.end();
          return;
        default:
          this.fail(new TenantError(`unknown frame type ${f.type}`, 0));
          this.socket.destroy();
          return;
      }
      if (this.settled) return;
    }
  }

  private fail(e: Error): void {
    if (this.settled) return;
    this.settled = true;
    this.rejectDone(e);
  }

  private write(buf: Buffer): Promise<void> {
    if (this.socket.destroyed || !this.socket.writable) return Promise.resolve();
    return new Promise((resolve) => {
      this.socket.write(buf, () => resolve());
    });
  }

  /** writeStdin sends data as stdin frames of at most 1 MiB, waiting for each to flush. */
  async writeStdin(data: Uint8Array): Promise<void> {
    for (let off = 0; off < data.byteLength; off += MAX_FRAME) {
      await this.write(encodeFrame(FRAME.stdin, data.subarray(off, Math.min(off + MAX_FRAME, data.byteLength))));
    }
  }

  endStdin(): Promise<void> {
    return this.write(encodeFrame(FRAME.stdinEOF));
  }

  /** stop asks the tenant API to end the process group: TERM, KILL after 5 s. */
  stop(): Promise<void> {
    return this.write(encodeFrame(FRAME.stop));
  }

  /** close drops the connection; the tenant API cancels the command. */
  close(): void {
    this.socket.destroy();
  }

  pause(): void {
    this.socket.pause();
  }

  resume(): void {
    this.socket.resume();
  }
}

export class TenantClient {
  socketPath: string;

  constructor(socketPath: string = TENANT_SOCKET) {
    this.socketPath = socketPath;
  }

  request(method: string, path: string, body?: unknown): Promise<Reply> {
    const payload = body === undefined ? undefined : Buffer.from(JSON.stringify(body));
    return new Promise((resolve, reject) => {
      const req = http.request(
        {
          socketPath: this.socketPath,
          agent: false,
          method,
          path,
          host: "tenant",
          headers: payload ? { "content-type": "application/json", "content-length": payload.length } : {},
          timeout: REQUEST_TIMEOUT_MS,
        },
        (res) => {
          const chunks: Buffer[] = [];
          let size = 0;
          res.on("data", (c: Buffer) => {
            size += c.length;
            if (size > MAX_RESPONSE) {
              req.destroy(new TenantError("tenant API response too large", 0));
              return;
            }
            chunks.push(c);
          });
          res.on("end", () => {
            const text = Buffer.concat(chunks).toString("utf8");
            let parsed: unknown = null;
            if (text.length > 0) {
              try {
                parsed = JSON.parse(text);
              } catch {
                reject(new TenantError(`tenant API ${method} returned invalid JSON (status ${res.statusCode})`, res.statusCode ?? 0));
                return;
              }
            }
            resolve({ status: res.statusCode ?? 0, body: parsed });
          });
          res.on("error", reject);
        },
      );
      req.on("timeout", () => req.destroy(new TenantError("tenant API request timed out", 0)));
      req.on("error", (e) => reject(e instanceof TenantError ? e : new TenantError(`tenant API: ${printable(e.message)}`, 0)));
      req.end(payload);
    });
  }

  private static fail(what: string, r: Reply): TenantError {
    const msg = errorText(r.body);
    return new TenantError(`tenant API ${what}: status ${r.status}${msg ? `: ${msg}` : ""}`, r.status);
  }

  private static checkId(id: string): void {
    if (!SANDBOX_ID_RE.test(id)) throw new TenantError("invalid sandbox id", 0);
  }

  async whoami(): Promise<WhoAmI> {
    const r = await this.request("GET", "/v1/tenant/whoami");
    if (r.status !== 200) throw TenantClient.fail("whoami", r);
    const b = r.body;
    if (!isObject(b) || typeof b.project !== "string" || !PROJECT_RE.test(b.project) || typeof b.peer_uid !== "number") {
      throw new TenantError("tenant API whoami: malformed answer", 0);
    }
    return { project: b.project, peerUid: b.peer_uid };
  }

  async list(): Promise<TenantSandbox[]> {
    const r = await this.request("GET", "/v1/tenant/sandboxes");
    if (r.status !== 200) throw TenantClient.fail("list sandboxes", r);
    if (!Array.isArray(r.body)) throw new TenantError("tenant API list: not a list", 0);
    return r.body.map(parseSandbox);
  }

  /** create makes a sandbox; the answer comes once it runs a command. */
  async create(): Promise<TenantSandbox> {
    const r = await this.request("POST", "/v1/tenant/sandboxes", {});
    if (r.status !== 201) throw TenantClient.fail("create sandbox", r);
    return parseSandbox(r.body);
  }

  /** get returns the sandbox, or null when the tenant has no such sandbox. */
  async get(id: string): Promise<TenantSandbox | null> {
    TenantClient.checkId(id);
    const r = await this.request("GET", `/v1/tenant/sandboxes/${id}`);
    if (r.status === 404) return null;
    if (r.status !== 200) throw TenantClient.fail("get sandbox", r);
    return parseSandbox(r.body);
  }

  async start(id: string): Promise<void> {
    TenantClient.checkId(id);
    const r = await this.request("POST", `/v1/tenant/sandboxes/${id}/start`);
    if (r.status !== 204) throw TenantClient.fail("start sandbox", r);
  }

  async stop(id: string): Promise<void> {
    TenantClient.checkId(id);
    const r = await this.request("POST", `/v1/tenant/sandboxes/${id}/stop`);
    if (r.status !== 204) throw TenantClient.fail("stop sandbox", r);
  }

  /** remove deletes a sandbox. It returns false when it was already gone. */
  async remove(id: string): Promise<boolean> {
    TenantClient.checkId(id);
    const r = await this.request("DELETE", `/v1/tenant/sandboxes/${id}`);
    if (r.status === 404) return false;
    if (r.status !== 204) throw TenantClient.fail("delete sandbox", r);
    return true;
  }

  /** openExec starts a command; the stream is ready once the server answered 101. */
  openExec(id: string, spec: TenantExecSpec, handlers: ExecHandlers): Promise<TenantExec> {
    if (!SANDBOX_ID_RE.test(id)) return Promise.reject(new TenantError("invalid sandbox id", 0));
    const body = Buffer.from(
      JSON.stringify({
        argv: spec.argv,
        env: spec.env ?? {},
        workdir: spec.workdir,
        timeout_seconds: spec.timeoutSeconds,
        stdin: spec.stdin,
        stdout_limit: spec.stdoutLimit,
        stderr_limit: spec.stderrLimit,
      }),
    );
    return new Promise((resolve, reject) => {
      let answered = false;
      const req = http.request({
        socketPath: this.socketPath,
        agent: false,
        method: "POST",
        path: `/v1/tenant/sandboxes/${id}/exec`,
        host: "tenant",
        headers: {
          connection: "Upgrade",
          upgrade: EXEC_PROTOCOL,
          "content-type": "application/json",
          "content-length": body.length,
        },
        timeout: REQUEST_TIMEOUT_MS,
      });
      req.on("upgrade", (res, socket, head) => {
        answered = true;
        if (res.statusCode !== 101 || String(res.headers.upgrade ?? "").toLowerCase() !== EXEC_PROTOCOL) {
          socket.destroy();
          reject(new TenantError(`tenant API exec: unexpected upgrade (status ${res.statusCode})`, res.statusCode ?? 0));
          return;
        }
        resolve(new TenantExec(socket, head, handlers));
      });
      req.on("response", (res) => {
        answered = true;
        const chunks: Buffer[] = [];
        let size = 0;
        res.on("data", (c: Buffer) => {
          size += c.length;
          if (size <= MAX_RESPONSE) chunks.push(c);
        });
        res.on("end", () => {
          let parsed: unknown = null;
          try {
            parsed = JSON.parse(Buffer.concat(chunks).toString("utf8"));
          } catch {
            // The status says enough.
          }
          reject(TenantClient.fail("exec", { status: res.statusCode ?? 0, body: parsed }));
        });
        res.on("error", reject);
      });
      req.on("timeout", () => {
        if (!answered) req.destroy(new TenantError("tenant API exec request timed out", 0));
      });
      req.on("error", (e) => {
        if (!answered) reject(e instanceof TenantError ? e : new TenantError(`tenant API: ${printable(e.message)}`, 0));
      });
      req.end(body);
    });
  }
}
