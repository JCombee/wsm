// The tenant client and the exec layer against a fake tenant API on a Unix
// socket. Where Unix sockets are not allowed (a build sandbox), the tests skip.
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { promises as fs } from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import type { Duplex } from "node:stream";
import { test, type TestContext } from "node:test";
import { ExecError, runExec, runStreaming } from "../src/exec.ts";
import { encodeFrame, FRAME, FrameDecoder, TenantClient, type Frame } from "../src/tenant.ts";

interface WireSpec {
  argv: string[];
  env: Record<string, string>;
  workdir?: string;
  timeout_seconds: number;
  stdin: boolean;
  stdout_limit?: number;
}

interface Fake {
  client: TenantClient;
  specs: WireSpec[];
  cancelled: string[];
}

const KNOWN = "tabcdefghij";
const CREATED = "tnewsandbx1";

function send(res: http.ServerResponse, status: number, body?: unknown): void {
  const b = body === undefined ? "" : JSON.stringify(body);
  res.writeHead(status, body === undefined ? {} : { "content-type": "application/json", "content-length": Buffer.byteLength(b) });
  res.end(b);
}

function handleExec(spec: WireSpec, socket: Duplex, rest: Buffer, cancelled: string[]): void {
  const prog = spec.argv[0] ?? "";
  if (prog === "refuse") {
    const b = JSON.stringify({ error: "invalid exec spec: argv holds a NUL byte" });
    socket.end(`HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: ${Buffer.byteLength(b)}\r\nConnection: close\r\n\r\n${b}`);
    return;
  }
  socket.write("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: wsm-exec.v1\r\n\r\n");
  const out = (type: number, payload: string | Buffer = "") => socket.write(encodeFrame(type, typeof payload === "string" ? Buffer.from(payload) : payload));
  let ended = false;
  const exit = (o: object) => {
    ended = true;
    out(FRAME.exit, JSON.stringify(o));
    socket.end();
  };
  // Like the tenant API: a dropped connection cancels. The HTTP server keeps
  // connections half-open, so the client's end shows up as "end".
  const drop = () => {
    if (!ended) {
      ended = true;
      cancelled.push(prog);
    }
    socket.destroy();
  };
  socket.on("end", drop);
  socket.on("close", drop);
  let onFrame: (f: Frame) => void = () => {};
  const dec = new FrameDecoder();
  socket.on("data", (c: Buffer) => {
    for (const f of dec.push(c)) onFrame(f);
  });
  socket.on("error", () => {});
  switch (prog) {
    case "exit7":
      out(FRAME.stdout, "out\n");
      out(FRAME.stderr, "err\n");
      exit({ code: 7, timed_out: false, stopped: false, reason: "exit" });
      break;
    case "cat":
      onFrame = (f) => {
        if (f.type === FRAME.stdin) out(FRAME.stdout, Buffer.from(f.payload));
        if (f.type === FRAME.stdinEOF) {
          out(FRAME.stderr, JSON.stringify(Object.keys(spec.env).sort()));
          exit({ code: 0, timed_out: false, stopped: false, reason: "exit" });
        }
      };
      break;
    case "flood":
      out(FRAME.stdout, "x".repeat(100));
      out(FRAME.error, `stdout exceeds the limit of ${spec.stdout_limit} bytes`);
      exit({ code: 143, timed_out: false, stopped: false, reason: "limit" });
      break;
    case "big":
      for (let i = 0; i < 3; i++) out(FRAME.stdout, Buffer.alloc(1 << 20, 0x61 + i));
      exit({ code: 0, timed_out: false, stopped: false, reason: "exit" });
      break;
    case "hang":
      onFrame = (f) => {
        if (f.type === FRAME.stop) exit({ code: null, timed_out: false, stopped: true, reason: "stop" });
      };
      break;
    default:
      exit({ code: 127, timed_out: false, stopped: false, reason: "exit" });
  }
  for (const f of dec.push(rest)) onFrame(f);
}

async function fakeTenant(t: TestContext): Promise<Fake | null> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "s07-tenant-"));
  const socketPath = path.join(dir, "tenant.sock");
  const specs: WireSpec[] = [];
  const cancelled: string[] = [];
  const known = new Set([KNOWN]);
  // Upgraded sockets leave the HTTP server's bookkeeping: track them for cleanup.
  const upgraded = new Set<Duplex>();
  const server = http.createServer((req, res) => {
    const url = req.url ?? "";
    const m = /^\/v1\/tenant\/sandboxes\/([^/]+)$/.exec(url);
    req.resume();
    if (req.method === "GET" && url === "/v1/tenant/whoami") return send(res, 200, { project: "s07", peer_uid: 61101, peer_gid: 61101, peer_pid: 1 });
    if (req.method === "GET" && url === "/v1/tenant/sandboxes") return send(res, 200, [...known].map((id) => ({ id, status: "running", created_at: "2026-11-24T00:00:00Z" })));
    if (req.method === "POST" && url === "/v1/tenant/sandboxes") {
      known.add(CREATED);
      return send(res, 201, { id: CREATED, status: "running", created_at: "2026-11-24T00:00:00Z" });
    }
    if (m && req.method === "GET") return known.has(m[1] ?? "") ? send(res, 200, { id: m[1], status: "running" }) : send(res, 404, { error: "no such sandbox" });
    if (m && req.method === "DELETE") return known.delete(m[1] ?? "") ? send(res, 204) : send(res, 404, { error: "no such sandbox" });
    send(res, 404, { error: "Not Found" });
  });
  server.on("upgrade", (req: http.IncomingMessage, socket: Duplex, head: Buffer) => {
    upgraded.add(socket);
    socket.on("close", () => upgraded.delete(socket));
    const len = Number(req.headers["content-length"] ?? 0);
    let body = Buffer.from(head);
    const proceed = () => {
      const spec = JSON.parse(body.subarray(0, len).toString("utf8")) as WireSpec;
      specs.push(spec);
      handleExec(spec, socket, body.subarray(len), cancelled);
    };
    if (body.length >= len) {
      proceed();
      return;
    }
    const onData = (c: Buffer) => {
      body = Buffer.concat([body, c]);
      if (body.length >= len) {
        socket.off("data", onData);
        proceed();
      }
    };
    socket.on("data", onData);
  });
  try {
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(socketPath, () => resolve());
    });
  } catch (e) {
    await fs.rm(dir, { recursive: true, force: true });
    const code = (e as NodeJS.ErrnoException).code;
    if (code === "EPERM" || code === "EACCES") {
      t.skip("Unix sockets are not allowed here");
      return null;
    }
    throw e;
  }
  t.after(async () => {
    server.closeAllConnections();
    for (const s of upgraded) s.destroy();
    await new Promise((resolve) => server.close(resolve));
    await fs.rm(dir, { recursive: true, force: true });
  });
  return { client: new TenantClient(socketPath), specs, cancelled };
}

const target = { sandbox: KNOWN };

test("S07 tenant client JSON routes", async (t) => {
  const f = await fakeTenant(t);
  if (!f) return;
  assert.deepEqual(await f.client.whoami(), { project: "s07", peerUid: 61101 });
  assert.deepEqual(await f.client.list(), [{ id: KNOWN, status: "running" }]);
  assert.deepEqual(await f.client.create(), { id: CREATED, status: "running" });
  assert.equal(await f.client.get("tzzzzzzzzzz"), null);
  assert.equal(await f.client.remove(CREATED), true);
  assert.equal(await f.client.remove(CREATED), false);
  await assert.rejects(f.client.get("../etc"), /invalid sandbox id/);
});

test("S07 exec returns stdout, stderr and the exit code", async (t) => {
  const f = await fakeTenant(t);
  if (!f) return;
  const r = await runExec({ target, command: "exit7", timeoutMs: 5000 }, { client: f.client });
  assert.equal(r.exitCode, 7);
  assert.equal(r.stdout.toString(), "out\n");
  assert.equal(r.stderr.toString(), "err\n");
  assert.equal(r.timedOut, false);
  assert.equal(f.specs[0]?.stdin, false);
});

test("S07 exec streams stdin and passes env as data", async (t) => {
  const f = await fakeTenant(t);
  if (!f) return;
  const input = randomBytes(3 * (1 << 20) + 17);
  const env = { MULTI: "a=b\n'c' \"d\"", EMPTY: "" };
  const r = await runExec({ target, command: "cat", env, timeoutMs: 5000 }, { client: f.client, stdin: input });
  assert.equal(r.exitCode, 0);
  assert.ok(r.stdout.equals(input));
  assert.deepEqual(JSON.parse(r.stderr.toString()), ["EMPTY", "MULTI"]);
  const s = f.specs[0];
  assert.deepEqual(s?.argv, ["cat"]);
  assert.deepEqual(s?.env, env);
  assert.equal(s?.stdin, true);
});

test("S07 exec refusal carries the tenant API's reason", async (t) => {
  const f = await fakeTenant(t);
  if (!f) return;
  await assert.rejects(runExec({ target, command: "refuse", timeoutMs: 5000 }, { client: f.client }), (e: unknown) => {
    assert.ok(e instanceof ExecError);
    assert.match(e.message, /status 400: invalid exec spec: argv holds a NUL byte/);
    return true;
  });
});

test("S07 exec reports the tenant API's output limit", async (t) => {
  const f = await fakeTenant(t);
  if (!f) return;
  await assert.rejects(runExec({ target, command: "flood", timeoutMs: 5000 }, { client: f.client, outputLimit: 1 << 20 }), /stdout exceeds the limit of 1048576 bytes/);
});

test("S07 streaming exec delivers stdout as a stream", async (t) => {
  const f = await fakeTenant(t);
  if (!f) return;
  const run = runStreaming({ target, command: "big", timeoutMs: 5000 }, { client: f.client });
  let n = 0;
  for await (const c of run.stdout) n += (c as Buffer).length;
  const r = await run.done;
  assert.equal(n, 3 << 20);
  assert.equal(r.exitCode, 0);
});

test("S07 stop ends a command and a dropped connection cancels one", async (t) => {
  const f = await fakeTenant(t);
  if (!f) return;
  const h = { stdout() {}, stderr() {} };
  const a = await f.client.openExec(KNOWN, { argv: ["hang"], stdin: true, timeoutSeconds: 10 }, h);
  await a.stop();
  const info = await a.done;
  assert.equal(info.stopped, true);
  assert.equal(info.reason, "stop");
  const b = await f.client.openExec(KNOWN, { argv: ["hang"], stdin: true, timeoutSeconds: 10 }, h);
  b.close();
  await assert.rejects(b.done);
  for (let i = 0; i < 50 && f.cancelled.length === 0; i++) await new Promise((r) => setTimeout(r, 20));
  assert.deepEqual(f.cancelled, ["hang"]);
});
