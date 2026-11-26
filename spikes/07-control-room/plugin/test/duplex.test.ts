import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { test } from "node:test";
import { ARG_ENV_PREFIX, DuplexChannels, DuplexError, WRAPPER, type DuplexExec, type DuplexSink } from "../src/duplex.ts";
import type { ExecHandlers, ExitInfo, TenantExecSpec } from "../src/tenant.ts";

interface FakeExec extends DuplexExec {
  spec: TenantExecSpec;
  handlers: ExecHandlers;
  written: Buffer[];
  stops: number;
  closes: number;
  finish(info: ExitInfo): void;
}

function harness() {
  const execs: FakeExec[] = [];
  const events: string[] = [];
  const logs: string[] = [];
  const sink: DuplexSink = {
    data(route, session, chunk) {
      events.push(`data ${route} ${session.slice(0, 7)} ${Buffer.from(chunk).toString()}`);
    },
    exit(route, _session, code, transportClosed) {
      events.push(`exit ${route} ${code} ${transportClosed ?? "-"}`);
    },
  };
  const channels = new DuplexChannels({
    sink,
    log: { info: (m) => logs.push(m), warn: (m) => logs.push(m) },
    cwd: "/workspace/paperclip",
    deps: {
      async open(_sandbox, spec, handlers) {
        let finish: (i: ExitInfo) => void = () => {};
        const done = new Promise<ExitInfo>((resolve) => {
          finish = resolve;
        });
        const ex: FakeExec = {
          spec,
          handlers,
          written: [],
          stops: 0,
          closes: 0,
          done,
          finish: (i) => finish(i),
          async writeStdin(d) {
            ex.written.push(Buffer.from(d));
          },
          async stop() {
            ex.stops++;
            setImmediate(() => finish({ code: null, timedOut: false, stopped: true, reason: "stop" }));
          },
          close() {
            ex.closes++;
          },
        };
        execs.push(ex);
        return ex;
      },
    },
  });
  return { channels, execs, events, logs };
}

const lease = { leaseId: "wsm:tabcdefghij", sandbox: "tabcdefghij" };
const tick = () => new Promise((r) => setImmediate(r));

test("S07 duplex wrapper rebuilds argv from env", () => {
  const env = { PATH: process.env.PATH ?? "/usr/bin:/bin", [`${ARG_ENV_PREFIX}0`]: "printf", [`${ARG_ENV_PREFIX}1`]: "%s|", [`${ARG_ENV_PREFIX}2`]: "a 'b' $c\nd" };
  const r = spawnSync("sh", ["-c", WRAPPER, "s07-duplex", "3"], { env, encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout, "a 'b' $c\nd|");
});

test("S07 duplex argv holds no command values", async () => {
  const h = harness();
  const secret = "PAPERCLIP_BRIDGE_TOKEN=s07-secret-value";
  const r = await h.channels.open({ hostRouteId: "route-1", command: ["env", secret, "node", "/x.js"] }, lease);
  assert.equal(r.hostRouteId, "route-1");
  const spec = h.execs[0]?.spec;
  assert.ok(spec);
  assert.deepEqual(spec.argv, ["sh", "-c", WRAPPER, "s07-duplex", "4"]);
  assert.ok(!spec.argv.join(" ").includes("s07-secret-value"));
  assert.equal(spec.env?.[`${ARG_ENV_PREFIX}1`], secret);
  assert.equal(spec.stdin, true);
  assert.ok(!h.logs.join("\n").includes("s07-secret-value"));
  await assert.rejects(h.channels.open({ hostRouteId: "route-1", command: ["x"] }, lease), DuplexError);
  await assert.rejects(h.channels.open({ hostRouteId: "route-2", command: [] }, lease), DuplexError);
  await assert.rejects(h.channels.open({ hostRouteId: "route-3", command: ["-x"] }, lease), DuplexError);
});

test("S07 duplex relays stdout, keeps stderr out of the channel and reports one exit", async () => {
  const h = harness();
  const { workerSessionId } = await h.channels.open({ hostRouteId: "r", command: ["cat"] }, lease);
  const ex = h.execs[0];
  assert.ok(ex);
  ex.handlers.stdout(Buffer.from("hello"));
  ex.handlers.stderr(Buffer.from("secret-ish noise"));
  await h.channels.write("r", workerSessionId, Buffer.from("in"));
  await h.channels.write("r", "duplex-other", Buffer.from("ignored"));
  assert.deepEqual(ex.written.map((b) => b.toString()), ["in"]);
  ex.finish({ code: 0, timedOut: false, stopped: false, reason: "exit" });
  await tick();
  ex.finish({ code: 1, timedOut: false, stopped: false, reason: "exit" });
  await tick();
  assert.deepEqual(h.events, [`data r ${workerSessionId.slice(0, 7)} hello`, "exit r 0 -"]);
  await h.channels.write("r", workerSessionId, Buffer.from("late"));
  assert.equal(ex.written.length, 1);
});

test("S07 duplex stop goes through the tenant API and close is idempotent", async () => {
  const h = harness();
  const { workerSessionId } = await h.channels.open({ hostRouteId: "r", command: ["sleep", "100"] }, lease);
  const ex = h.execs[0];
  assert.ok(ex);
  await h.channels.stop("r", workerSessionId);
  assert.equal(ex.stops, 1);
  assert.deepEqual(h.events, ["exit r null true"]);
  const a = await h.channels.close("r");
  const b = await h.channels.close("r");
  assert.deepEqual(a, { hostRouteId: "r", workerSessionId });
  assert.deepEqual(b, { hostRouteId: "r", workerSessionId });
  assert.equal(ex.stops, 1);
  assert.equal(ex.closes, 1);
  await assert.rejects(h.channels.open({ hostRouteId: "r", command: ["x"] }, lease), DuplexError);
  assert.equal(h.channels.size, 0);
});

test("S07 duplex lease end drops the connection", async () => {
  const h = harness();
  await h.channels.open({ hostRouteId: "r1", command: ["cat"] }, lease);
  await h.channels.open({ hostRouteId: "r2", command: ["cat"] }, { leaseId: "wsm:tzzzzzzzzzz", sandbox: "tzzzzzzzzzz" });
  h.channels.closeLease(lease.leaseId);
  assert.equal(h.execs[0]?.closes, 1);
  assert.equal(h.execs[1]?.closes, 0);
  assert.equal(h.channels.size, 1);
});
