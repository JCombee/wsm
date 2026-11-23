// Spike 05: checks the plugin's exec layer against a real wsm sandbox.
// Usage, as pclip: node exec-check.mjs <plugin>/dist/testkit.js <project>
// Prints RESULT<TAB>ID<TAB>PASS|FAIL|INFO<TAB>detail lines on stdout. It creates
// its own sandbox through the management API and deletes it at the end.
import { createHash, randomBytes } from "node:crypto";

const [kitPath, project] = process.argv.slice(2);
const kit = await import(kitPath);

const out = (id, status, detail) => process.stdout.write(`RESULT\t${id}\t${status}\t${detail}\n`);
const check = (id, ok, detail) => out(id, ok ? "PASS" : "FAIL", detail);
const log = { info: (m) => process.stderr.write(`${m}\n`), warn: (m) => process.stderr.write(`WARN ${m}\n`) };
const sha = (b) => createHash("sha256").update(b).digest("hex");

const admin = new kit.AdminClient();
const sandboxes = new kit.Sandboxes(admin, log);

let t0 = Date.now();
const lease = await sandboxes.acquire(project);
out("acquire-ms", "INFO", `${Date.now() - t0}`);
const target = lease.target;
const run = (spec, opts) => kit.runExec({ target, timeoutMs: 30_000, ...spec }, { log, ...opts });

async function step(id, fn) {
  try {
    await fn();
  } catch (e) {
    out(id, "FAIL", `error: ${e instanceof Error ? e.message : String(e)}`.slice(0, 300));
  }
}

try {
  await step("EXEC-EXIT", async () => {
    const r = await run({ command: "sh", args: ["-c", "echo out; echo err >&2; exit 7"] });
    check("EXEC-EXIT", r.exitCode === 7 && r.stdout.toString() === "out\n" && r.stderr.toString() === "err\n",
      `exit=${r.exitCode} stdout=${JSON.stringify(r.stdout.toString())} stderr=${JSON.stringify(r.stderr.toString())}`);
  });

  await step("exec-latency", async () => {
    const t = Date.now();
    for (let i = 0; i < 10; i++) await run({ command: "true" });
    out("exec-latency-ms", "INFO", `${Math.round((Date.now() - t) / 10)} per exec (10 sequential true)`);
  });

  await step("EXEC-STDIN", async () => {
    // 8 MiB of text: base64 lines of random bytes.
    const text = Buffer.from(randomBytes(6 << 20).toString("base64").replace(/(.{76})/g, "$1\n"));
    const input = text.subarray(0, 8 << 20);
    const r = await run({ command: "cat", interactive: true }, { stdin: input });
    check("EXEC-STDIN", r.exitCode === 0 && sha(r.stdout) === sha(input),
      `bytes=${input.length} back=${r.stdout.length} sha-match=${sha(r.stdout) === sha(input)} exit=${r.exitCode}`);
  });

  await step("EXEC-ENV", async () => {
    const multi = 'line one\nKEY="a=b c"\n\'single\' $HOME `x`\n=\n';
    const env = { S05_MULTI: multi, S05_EMPTY: "", CONTAINER_HOST: "unix:///tmp/nope.sock" };
    const script = 'printf %s "$S05_MULTI" | sha256sum | cut -c1-64; printf "[%s]\\n" "${S05_EMPTY-unset}"; printf "%s\\n" "$CONTAINER_HOST"; printf "%s\\n" "${S05_UNSET-unset}"';
    const r = await run({ command: "sh", args: ["-c", script], env });
    const lines = r.stdout.toString().split("\n");
    const ok = r.exitCode === 0 && lines[0] === sha(multi) && lines[1] === "[]" &&
      lines[2] === "unix:///tmp/nope.sock" && lines[3] === "unset";
    check("EXEC-ENV", ok, `exit=${r.exitCode} multiline-sha-match=${lines[0] === sha(multi)} empty=${lines[1]} container_host=${lines[2]} unset=${lines[3]}`);
  });

  await step("EXEC-TIMEOUT", async () => {
    const t = Date.now();
    const r = await run({ command: "sleep", args: ["30"], timeoutMs: 3000 });
    const took = Date.now() - t;
    await new Promise((res) => setTimeout(res, 1000));
    const ps = await run({ command: "sh", args: ["-c", 'for p in /proc/[0-9]*; do tr "\\0" " " <"$p/cmdline" 2>/dev/null; echo; done'] });
    const left = ps.stdout.toString().split("\n").filter((l) => l.startsWith("sleep 30")).length;
    check("EXEC-TIMEOUT", r.timedOut === true && took < 15_000 && left === 0,
      `timedOut=${r.timedOut} exit=${r.exitCode} took=${took}ms sleep-30-left=${left}`);
  });

  await step("EXEC-PARALLEL", async () => {
    const t = Date.now();
    const rs = await Promise.all([0, 1, 2, 3].map(() => run({ command: "sleep", args: ["3"] })));
    const took = Date.now() - t;
    check("EXEC-PARALLEL", rs.every((r) => r.exitCode === 0) && took < 8000, `4 x sleep 3 took ${took}ms exits=${rs.map((r) => r.exitCode).join(",")}`);
  });

  await step("EXEC-LIMIT", async () => {
    const t = Date.now();
    let err = null;
    try {
      await run({ command: "head", args: ["-c", "4194304", "/dev/zero"] }, { outputLimit: 1 << 20 });
    } catch (e) {
      err = e;
    }
    const took = Date.now() - t;
    check("EXEC-LIMIT", err !== null && /exceeds the limit/.test(err.message) && took < 30_000,
      `error=${err ? JSON.stringify(err.message) : "none"} took=${took}ms`);
  });
} finally {
  t0 = Date.now();
  await sandboxes.release(lease.leaseId, project, "release");
  const left = await admin.sandbox(project, lease.sandbox);
  out("release-ms", "INFO", `${Date.now() - t0} sandbox-left=${left ? "yes" : "no"}`);
}
