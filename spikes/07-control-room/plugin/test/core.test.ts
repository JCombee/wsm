import assert from "node:assert/strict";
import { test } from "node:test";
import { ConfigError, DEFAULT_TIMEOUT_MS, MAX_TIMEOUT_MS, parseConfig } from "../src/config.ts";
import { ExecError, toTenantSpec, type ExecSpec } from "../src/exec.ts";
import { formatLeaseId, LeaseError, parseLeaseId } from "../src/lease.ts";
import { encodeFrame, FRAME, FrameDecoder, MAX_FRAME, parseExit, parseSandbox, TenantError } from "../src/tenant.ts";

const target = { sandbox: "tabcdefghij" };

function spec(over: Partial<ExecSpec> = {}): ExecSpec {
  return { target, command: "sh", args: ["-c", "echo hi"], timeoutMs: 5000, ...over };
}

test("S07 config holds only a timeout", () => {
  const cases: Array<[unknown, { timeoutMs: number }]> = [
    [{}, { timeoutMs: DEFAULT_TIMEOUT_MS }],
    [{ timeoutMs: 60000 }, { timeoutMs: 60000 }],
    [{ timeoutMs: MAX_TIMEOUT_MS }, { timeoutMs: MAX_TIMEOUT_MS }],
    [{ reuseLease: false, archiveOnRelease: true }, { timeoutMs: DEFAULT_TIMEOUT_MS }],
  ];
  for (const [raw, want] of cases) assert.deepEqual(parseConfig(raw), want);
});

test("S07 config rejects a project, secrets and unknown keys", () => {
  const bad: unknown[] = [
    null, [], "x", { project: "s07" }, { apiKey: "x" }, { socket: "/run/wsm/api.sock" },
    { reuseLease: true }, { archiveOnRelease: "yes" }, { timeoutMs: "5" }, { timeoutMs: 5 },
    { timeoutMs: MAX_TIMEOUT_MS + 1 }, { timeoutMs: 1500.5 },
  ];
  for (const raw of bad) assert.throws(() => parseConfig(raw), ConfigError, JSON.stringify(raw));
  const extraKeys: string[] = [];
  assert.deepEqual(parseConfig({ agentId: "a", sandbox: true }, { strict: false, extraKeys }), { timeoutMs: DEFAULT_TIMEOUT_MS });
  assert.deepEqual(extraKeys.sort(), ["agentId", "sandbox"]);
  assert.throws(() => parseConfig({ timeoutMs: 5 }, { strict: false }), ConfigError);
});

test("S07 lease id round trip and refusals", () => {
  assert.equal(formatLeaseId("tabcdefghij"), "wsm:tabcdefghij");
  assert.equal(parseLeaseId("wsm:tabcdefghij"), "tabcdefghij");
  for (const bad of ["", "wsm:", "wsm:pc-0123456789ab", "wsm:s07/tabcdefghij", "wsm:tABCDEFGHIJ", "wsm:tabcdefghijk", "tabcdefghij", 5, null]) {
    assert.throws(() => parseLeaseId(bad), LeaseError, String(bad));
  }
  assert.throws(() => formatLeaseId("pc-0123456789ab"), LeaseError);
});

test("S07 exec spec carries argv and env as data", () => {
  const t = toTenantSpec(spec({ env: { A: "x\ny=z 'q'", EMPTY: "" }, cwd: "/workspace/paperclip/" }), 1 << 20, true);
  assert.deepEqual(t.argv, ["sh", "-c", "echo hi"]);
  assert.deepEqual(t.env, { A: "x\ny=z 'q'", EMPTY: "" });
  assert.equal(t.workdir, "/workspace/paperclip");
  assert.equal(t.stdin, true);
  assert.equal(t.timeoutSeconds, 5);
  assert.equal(t.stdoutLimit, 1 << 20);
  assert.equal(toTenantSpec(spec({ timeoutMs: 1 }), 1, false).timeoutSeconds, 1);
  assert.equal(toTenantSpec(spec({ timeoutMs: 10 * 60 * 60 * 1000 }), 1, false).timeoutSeconds, 4 * 60 * 60);
  assert.equal(toTenantSpec(spec(), 1 << 30, false).stdoutLimit, 512 << 20);
});

test("S07 exec spec rejects bad commands, env and cwd", () => {
  const bad: Array<Partial<ExecSpec>> = [
    { command: "" }, { command: "-x" }, { command: "a\0b" }, { args: ["ok", "a\0"] },
    { env: { "1A": "x" } }, { env: { "A-B": "x" } }, { env: { A: "a\0" } }, { cwd: "relative" }, { timeoutMs: 0 }, { timeoutMs: Number.NaN },
  ];
  for (const over of bad) assert.throws(() => toTenantSpec(spec(over), 1, false), ExecError, JSON.stringify(over));
});

test("S07 frames round trip, also split and joined", () => {
  const frames = [encodeFrame(FRAME.stdout, Buffer.from("out")), encodeFrame(FRAME.exit, Buffer.from('{"code":0}')), encodeFrame(FRAME.stdinEOF)];
  const all = Buffer.concat(frames);
  for (const cut of [1, 3, 5, 7, all.length - 1]) {
    const d = new FrameDecoder();
    const got = [...d.push(all.subarray(0, cut)), ...d.push(all.subarray(cut))];
    assert.deepEqual(got.map((f) => [f.type, f.payload.toString()]), [[FRAME.stdout, "out"], [FRAME.exit, '{"code":0}'], [FRAME.stdinEOF, ""]]);
    assert.equal(d.pending, 0);
  }
  const big = Buffer.alloc(5);
  big[0] = FRAME.stdout;
  big.writeUInt32BE(MAX_FRAME + 1, 1);
  assert.throws(() => new FrameDecoder().push(big), TenantError);
  assert.throws(() => encodeFrame(FRAME.stdin, Buffer.alloc(MAX_FRAME + 1)), TenantError);
});

test("S07 exit and sandbox answers are checked", () => {
  assert.deepEqual(parseExit(Buffer.from('{"code":7,"timed_out":false,"stopped":false,"reason":"exit"}')), { code: 7, timedOut: false, stopped: false, reason: "exit" });
  assert.deepEqual(parseExit(Buffer.from('{"code":null,"stopped":true,"reason":"stop"}')), { code: null, timedOut: false, stopped: true, reason: "stop" });
  assert.equal(parseExit(Buffer.from('{"code":0,"reason":"ex\\u0007\\u001b[1;31]it\\r\\n"}')).reason, "exit");
  assert.equal(parseExit(Buffer.from(JSON.stringify({ code: 0, reason: "x".repeat(100) }))).reason, "x".repeat(32));
  for (const bad of ["x", "[]", '{"code":"7"}', '{"code":1.5}', '{"code":999}']) {
    assert.throws(() => parseExit(Buffer.from(bad)), TenantError, bad);
  }
  assert.deepEqual(parseSandbox({ id: "tabcdefghij", status: "running", created_at: "x" }), { id: "tabcdefghij", status: "running" });
  for (const bad of [{ id: "pc-1", status: "running" }, { id: "tabcdefghij", status: "Running!" }, { id: "tabcdefghij" }, null]) {
    assert.throws(() => parseSandbox(bad), TenantError, JSON.stringify(bad));
  }
});
