import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { PassThrough } from "node:stream";
import { test } from "node:test";
import {
  ARG_ENV_PREFIX,
  DuplexChannels,
  DuplexError,
  duplexSpec,
  STOP_SCRIPT,
  WRAPPER,
  type DuplexDeps,
  type DuplexSink,
} from "../src/duplex.ts";
import { buildInvocation, silentLog, type ExecResult, type ExecSpec } from "../src/exec.ts";

const target = { socket: "/run/wsm-podman/wsm-p-50000/podman.sock", container: "sbx-abcdef012345-work" };
const lease = { leaseId: "wsm:s05/pc-0123456789ab", sandbox: "pc-0123456789ab", target };
const TOKEN = "pcb_0123456789abcdefABCDEF0123456789xyz";
const bridgeCommand = ["sh", "-c", `exec env PAPERCLIP_BRIDGE_TOKEN='${TOKEN}' PAPERCLIP_BRIDGE_NONCE=abc 'node' '/w/.bridge/gateway.mjs'`];

const tick = () => new Promise((r) => setImmediate(r));

interface Finished {
  code: number | null;
  signal: string | null;
  stdout: string;
}

function runSh(args: string[], env: Record<string, string> = {}): { child: ChildProcess; done: Promise<Finished> } {
  const child = spawn("sh", args, { env: { PATH: process.env.PATH ?? "/usr/bin:/bin", ...env }, stdio: ["ignore", "pipe", "inherit"] });
  const out: Buffer[] = [];
  child.stdout?.on("data", (c: Buffer) => out.push(c));
  const done = new Promise<Finished>((resolve) => {
    child.on("close", (code, signal) => resolve({ code, signal, stdout: Buffer.concat(out).toString("utf8") }));
  });
  return { child, done };
}

function argEnv(command: readonly string[]): Record<string, string> {
  const env: Record<string, string> = {};
  command.forEach((v, i) => {
    env[`${ARG_ENV_PREFIX}${i}`] = v;
  });
  return env;
}

async function waitForFile(file: string): Promise<string> {
  for (let i = 0; i < 100; i++) {
    const s = await readFile(file, "utf8").catch(() => "");
    if (s.endsWith("\n")) return s.trim();
    await new Promise((r) => setTimeout(r, 20));
  }
  throw new Error(`${file} never appeared`);
}

class FakeChild extends EventEmitter {
  stdin = new PassThrough();
  stdout = new PassThrough();
  stderr = new PassThrough();
  exitCode: number | null = null;
  signalCode: NodeJS.Signals | null = null;
  killed: string[] = [];

  kill(sig: NodeJS.Signals): boolean {
    this.killed.push(sig);
    this.finish(null, sig);
    return true;
  }

  finish(code: number | null, signal: NodeJS.Signals | null): void {
    if (this.exitCode !== null || this.signalCode !== null) return;
    this.exitCode = code;
    this.signalCode = signal;
    setImmediate(() => this.emit("close", code, signal));
  }
}

function fakeDeps(stopExitCode = 143) {
  const children: FakeChild[] = [];
  const specs: ExecSpec[] = [];
  const runs: ExecSpec[] = [];
  const deps: DuplexDeps = {
    start(spec) {
      specs.push(spec);
      const child = new FakeChild();
      children.push(child);
      return { child: child as unknown as ChildProcess, invocation: buildInvocation(spec), startedAt: Date.now() };
    },
    async run(spec): Promise<ExecResult> {
      runs.push(spec);
      children.at(-1)?.finish(stopExitCode, null);
      return { exitCode: 0, signal: null, timedOut: false, stdout: Buffer.from("term\n"), stderr: Buffer.alloc(0), durationMs: 1 };
    },
  };
  return { deps, children, specs, runs };
}

test("S05 duplex wrapper rebuilds argv from env and records the pid", async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "s05-duplex-"));
  try {
    const pidFile = path.join(dir, "x.pid");
    const tricky = `it's "quoted" $HOME \`id\`\nline2`;
    const command = ["sh", "-c", `printf '%s\\0' "$$" "$1" "$2" "$(env | grep -c '^${ARG_ENV_PREFIX}')"`, "inner", "two words", tricky];
    const { child, done } = runSh(["-c", WRAPPER, "s05-duplex", pidFile, String(command.length)], argEnv(command));
    const r = await done;
    assert.equal(r.code, 0);
    const [pid, a1, a2, leftover] = r.stdout.split("\0");
    assert.equal(pid, String(child.pid), "the command runs in the wrapper's own process");
    assert.equal((await readFile(pidFile, "utf8")).trim(), pid);
    assert.equal(a1, "two words");
    assert.equal(a2, tricky);
    assert.equal(leftover, "0", "no S05_ARG_ variable reaches the command");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("S05 duplex stop script ends the process", async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "s05-duplex-"));
  try {
    const cases: Array<{ name: string; command: string[]; grace: string; want: string; signal: string }> = [
      { name: "term", command: ["sleep", "30"], grace: "5", want: "term", signal: "SIGTERM" },
      { name: "kill", command: ["sh", "-c", "trap '' TERM; while :; do sleep 0.1; done"], grace: "1", want: "kill", signal: "SIGKILL" },
    ];
    for (const c of cases) {
      const pidFile = path.join(dir, `${c.name}.pid`);
      const run = runSh(["-c", WRAPPER, "s05-duplex", pidFile, String(c.command.length)], argEnv(c.command));
      await waitForFile(pidFile);
      const stop = await runSh(["-c", STOP_SCRIPT, "s05-stop", pidFile, c.grace]).done;
      const r = await run.done;
      assert.equal(stop.code, 0, c.name);
      assert.equal(stop.stdout.trim(), c.want, c.name);
      assert.equal(r.signal, c.signal, c.name);
      await assert.rejects(readFile(pidFile), c.name);
    }
    for (const [name, content] of [["garbled", "12x\n"], ["empty", ""]] as const) {
      const pidFile = path.join(dir, `${name}.pid`);
      await writeFile(pidFile, content);
      const stop = await runSh(["-c", STOP_SCRIPT, "s05-stop", pidFile, "1"]).done;
      assert.equal(stop.code, 0, name);
      assert.equal(stop.stdout, "", name);
    }
    const missing = await runSh(["-c", STOP_SCRIPT, "s05-stop", path.join(dir, "missing.pid"), "1"]).done;
    assert.equal(missing.code, 0);
    assert.equal(missing.stdout, "");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("S05 duplex argv holds no command values", () => {
  const spec = duplexSpec(target, bridgeCommand, "/tmp/.s05-duplex-0011223344556677.pid", "/workspace/paperclip");
  const inv = buildInvocation(spec);
  for (const a of inv.argv) {
    assert.ok(!a.includes(TOKEN), "the bridge token is not in argv");
    assert.ok(!a.includes("PAPERCLIP_BRIDGE"), "no part of the host command is in argv");
  }
  assert.deepEqual(inv.envNames, [`${ARG_ENV_PREFIX}0`, `${ARG_ENV_PREFIX}1`, `${ARG_ENV_PREFIX}2`]);
  assert.equal(inv.env[`${ARG_ENV_PREFIX}2`], bridgeCommand[2]);
  const i = inv.argv.indexOf("exec");
  assert.deepEqual(inv.argv.slice(i, i + 2), ["exec", "-i"]);
  assert.equal(inv.argv.at(-1), "3");
  assert.equal(inv.argv.at(-2), "/tmp/.s05-duplex-0011223344556677.pid");
  for (const bad of [[], [""], ["-x"], ["sh", "a\0b"], [1], "sh", Array(65).fill("a")]) {
    assert.throws(() => new DuplexChannels({ sink: { data() {}, exit() {} }, log: silentLog, cwd: "/w", deps: fakeDeps().deps }).open({ hostRouteId: "r", command: bad }, lease), DuplexError);
  }
});

test("S05 duplex relays stdout, keeps stderr out of the channel and reports one exit", async () => {
  const { deps, children } = fakeDeps();
  const events: string[] = [];
  const logs: string[] = [];
  const sink: DuplexSink = {
    data(route, session, chunk) {
      events.push(`data ${route} ${session.startsWith("duplex-")} ${Buffer.from(chunk).toString("utf8")}`);
    },
    exit(route, _session, code, transportClosed) {
      events.push(`exit ${route} ${code} ${transportClosed ?? "-"}`);
    },
  };
  const log = { info: (m: string) => logs.push(m), warn: (m: string) => logs.push(m) };
  const channels = new DuplexChannels({ sink, log, cwd: "/workspace/paperclip", deps });
  const opened = channels.open({ hostRouteId: "route-1", command: bridgeCommand }, lease);
  assert.equal(opened.hostRouteId, "route-1");
  const child = children[0];
  assert.ok(child);
  const stdin: Buffer[] = [];
  child.stdin.on("data", (c: Buffer) => stdin.push(c));
  child.stdout.write("READY abc\n");
  child.stderr.write(`gateway failed token=${TOKEN}\n\x1b[31mred`);
  await tick();
  await channels.write("route-1", opened.workerSessionId, Buffer.from("PRI * HTTP/2.0"));
  await channels.write("route-1", "duplex-other", Buffer.from("wrong pair"));
  await channels.write("route-x", opened.workerSessionId, Buffer.from("wrong route"));
  child.finish(0, null);
  await tick();
  child.emit("close", 0, null);
  await tick();
  assert.deepEqual(events, ["data route-1 true READY abc\n", "exit route-1 0 -"]);
  assert.equal(Buffer.concat(stdin).toString("utf8"), "PRI * HTTP/2.0");
  const all = logs.join("\n");
  assert.match(all, /s05 duplex open sandbox=pc-0123456789ab route=[0-9a-f]{12} argv0="sh" args=2/);
  assert.match(all, /s05 duplex stderr .*gateway failed \[redacted\]/);
  assert.match(all, /s05 duplex exit .*code=0 /);
  assert.ok(!all.includes(TOKEN), "no token in the log");
  assert.ok(!all.includes("\x1b"), "no control bytes in the log");
  assert.ok(!all.includes("route-1"), "the host route ID is logged only as a tag");
});

test("S05 duplex stop signals in the container first and close is idempotent", async () => {
  const { deps, children, specs, runs } = fakeDeps();
  const events: string[] = [];
  const sink: DuplexSink = {
    data() {},
    exit(_route, _session, code, transportClosed) {
      events.push(`exit ${code} ${transportClosed ?? "-"}`);
    },
  };
  const channels = new DuplexChannels({ sink, log: silentLog, cwd: "/workspace/paperclip", deps });
  const opened = channels.open({ hostRouteId: "route-2", command: ["sleep", "1000"] }, lease);
  await channels.stop("route-2", "duplex-wrong");
  assert.equal(runs.length, 0, "a stop with the wrong pair stops nothing");
  await channels.stop("route-2", opened.workerSessionId);
  assert.equal(runs.length, 1);
  const stop = runs[0];
  assert.ok(stop);
  assert.equal(stop.args?.[1], STOP_SCRIPT);
  assert.equal(stop.args?.[3], specs[0]?.args?.[3], "stop reads the channel's own PID file");
  assert.equal(stop.args?.[4], "5");
  assert.deepEqual(children[0]?.killed, [], "the client ended by itself after the in-container kill");
  assert.deepEqual(events, ["exit 143 -"]);
  const a = await channels.close("route-2");
  const b = await channels.close("route-2");
  assert.deepEqual(a, { hostRouteId: "route-2", workerSessionId: opened.workerSessionId });
  assert.deepEqual(b, a);
  assert.deepEqual(await channels.close("never-opened"), { hostRouteId: "never-opened" });
  assert.throws(() => channels.open({ hostRouteId: "route-2", command: ["sh"] }, lease), DuplexError, "a used route never reopens");
  assert.equal(runs.length, 1, "close after the exit runs no second stop");
  assert.equal(channels.size, 0);

  // A lease release kills the client of a still-open channel.
  const second = channels.open({ hostRouteId: "route-3", command: ["sleep", "1000"] }, lease);
  channels.closeLease(lease.leaseId);
  await tick();
  assert.deepEqual(children[1]?.killed, ["SIGKILL"]);
  assert.deepEqual(events, ["exit 143 -", "exit null true"]);
  assert.deepEqual(await channels.close("route-3"), { hostRouteId: "route-3", workerSessionId: second.workerSessionId });
});
