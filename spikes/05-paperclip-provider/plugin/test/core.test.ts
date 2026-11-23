import assert from "node:assert/strict";
import { test } from "node:test";
import { AdminError, parseProject, parseSandbox, podmanSocketFor } from "../src/admin.ts";
import { ConfigError, DEFAULT_TIMEOUT_MS, parseConfig } from "../src/config.ts";
import { buildInvocation, ExecError, type ExecSpec } from "../src/exec.ts";
import { formatLeaseId, LeaseError, newSandboxSlug, parseLeaseId } from "../src/lease.ts";

const target = { socket: "/run/wsm-podman/wsm-p-50000/podman.sock", container: "sbx-abcdef012345-work" };

function spec(over: Partial<ExecSpec> = {}): ExecSpec {
  return { target, command: "sh", args: ["-c", "echo hi"], timeoutMs: 5000, ...over };
}

test("S05 config accepts a valid project", () => {
  const cases: Array<[unknown, { project: string; timeoutMs: number }]> = [
    [{ project: "s05" }, { project: "s05", timeoutMs: DEFAULT_TIMEOUT_MS }],
    [{ project: "a" }, { project: "a", timeoutMs: DEFAULT_TIMEOUT_MS }],
    [{ project: "my-proj-2", timeoutMs: 60000 }, { project: "my-proj-2", timeoutMs: 60000 }],
    [{ project: "a".repeat(32) }, { project: "a".repeat(32), timeoutMs: DEFAULT_TIMEOUT_MS }],
  ];
  for (const [raw, want] of cases) assert.deepEqual(parseConfig(raw), want);
});

test("S05 config rejects bad slugs and unknown keys", () => {
  const bad: unknown[] = [
    null, [], "s05", {},
    { project: "" }, { project: "S05" }, { project: "1abc" }, { project: "abc-" },
    { project: "a".repeat(33) }, { project: "../x" }, { project: "s05/x" }, { project: 5 },
    { project: "s05", extra: 1 }, { project: "s05", apiKey: "x" }, { project: "s05", provider: "wsm" },
    { project: "s05", timeoutMs: "5" }, { project: "s05", timeoutMs: 5 }, { project: "s05", timeoutMs: 1.5e3 + 0.5 },
    { project: "s05", timeoutMs: 1e12 },
  ];
  for (const raw of bad) assert.throws(() => parseConfig(raw), ConfigError, JSON.stringify(raw));
});

test("S05 lease id round trip", () => {
  for (let i = 0; i < 20; i++) {
    const ref = { project: "s05", sandbox: newSandboxSlug() };
    assert.match(ref.sandbox, /^pc-[0-9a-f]{12}$/);
    const id = formatLeaseId(ref);
    assert.equal(id, `wsm:s05/${ref.sandbox}`);
    assert.deepEqual(parseLeaseId(id, "s05"), ref);
  }
});

test("S05 lease id rejects other projects and malformed ids", () => {
  const bad: unknown[] = [
    "wsm:other/pc-0123456789ab",
    "wsm:s05/pc-0123456789aB", "wsm:s05/pc-0123456789a", "wsm:s05/pc-0123456789abc",
    "wsm:s05/sbx-0123456789ab", "wsm:s05/pc-0123456789ab/x", "wsm:s05/../pc-0123456789ab",
    "fake-plugin://run/1", "wsm:s05", "", null, 42, " wsm:s05/pc-0123456789ab", "wsm:s05/pc-0123456789ab\n",
  ];
  for (const id of bad) assert.throws(() => parseLeaseId(id, "s05"), LeaseError, String(id));
  assert.throws(() => formatLeaseId({ project: "s05", sandbox: "x" }), LeaseError);
});

test("S05 exec argv holds no env values", () => {
  const secret = "s3cr3t-value\nline2 with = and \"quotes\"";
  const inv = buildInvocation(spec({ env: { S05_SENTINEL: secret, PAPERCLIP_API_KEY: "tok-123", EMPTY: "" } }));
  for (const a of inv.argv) {
    assert.ok(!a.includes("s3cr3t"), `argv leaks a value: ${a}`);
    assert.ok(!a.includes("tok-123"), `argv leaks a value: ${a}`);
  }
  const i = inv.argv.indexOf("S05_SENTINEL");
  assert.ok(i > 0 && inv.argv[i - 1] === "-e");
  assert.equal(inv.env.S05_SENTINEL, secret);
  assert.equal(inv.env.PAPERCLIP_API_KEY, "tok-123");
  assert.equal(inv.env.EMPTY, "");
  assert.deepEqual(inv.envNames, ["EMPTY", "PAPERCLIP_API_KEY", "S05_SENTINEL"]);
  // The client env is minimal: only the fixed base plus the passed names.
  assert.deepEqual(Object.keys(inv.env).sort(), ["EMPTY", "HOME", "LANG", "PAPERCLIP_API_KEY", "PATH", "S05_SENTINEL"]);
  assert.equal(inv.env.PATH, "/usr/bin:/bin");
  assert.deepEqual(inv.argv.slice(0, 4), ["--remote", "--url", `unix://${target.socket}`, "exec"]);
  assert.ok(inv.argv.includes("--user") && inv.argv[inv.argv.indexOf("--user") + 1] === "1000:1000");
});

test("S05 exec passes podman-read env names as NAME=VALUE", () => {
  const env = {
    CONTAINER_HOST: "unix:///tmp/nope.sock", container_host: "x", PODMAN_X: "1", DOCKER_HOST: "tcp://h",
    BUILDAH_ISOLATION: "chroot", XDG_RUNTIME_DIR: "/run/user/1", LD_PRELOAD: "/x.so", SSL_CERT_FILE: "/c",
    HOME: "/home/agent", PATH: "/usr/local/bin:/usr/bin", TMPDIR: "/tmp", HTTPS_PROXY: "http://p:3128",
    http_proxy: "http://p:3128", no_proxy: "localhost", ALL_PROXY: "socks5://p",
  };
  const inv = buildInvocation(spec({ env }));
  for (const [k, v] of Object.entries(env)) {
    assert.ok(inv.argv.includes(`${k}=${v}`), `${k} should be NAME=VALUE in argv`);
  }
  // None of them steer the podman client: its env keeps only the fixed base.
  assert.deepEqual(Object.keys(inv.env).sort(), ["HOME", "LANG", "PATH"]);
  assert.equal(inv.env.PATH, "/usr/bin:/bin");
  assert.notEqual(inv.env.HOME, "/home/agent");
  // A newline or an @ (credentials in a proxy URL) is refused for these names.
  assert.throws(() => buildInvocation(spec({ env: { HTTPS_PROXY: "http://user:pw@proxy:3128" } })), ExecError);
  assert.throws(() => buildInvocation(spec({ env: { PATH: "/bin\n/x" } })), ExecError);
});

test("S05 exec rejects bad env names and NUL values", () => {
  for (const name of ["", "1A", "A-B", "A B", "A=B", "Ä", "A\0", "$A"]) {
    assert.throws(() => buildInvocation(spec({ env: { [name]: "v" } })), ExecError, JSON.stringify(name));
  }
  assert.throws(() => buildInvocation(spec({ env: { GOOD: "a\0b" } })), ExecError);
  assert.throws(() => buildInvocation(spec({ args: ["a\0b"] })), ExecError);
  assert.throws(() => buildInvocation(spec({ command: "" })), ExecError);
  assert.throws(() => buildInvocation(spec({ command: "--privileged" })), ExecError);
  assert.throws(() => buildInvocation(spec({ cwd: "relative" })), ExecError);
  assert.throws(() => buildInvocation(spec({ target: { ...target, container: "other" } })), ExecError);
  assert.throws(() => buildInvocation(spec({ target: { ...target, socket: "/var/run/docker.sock" } })), ExecError);
});

test("S05 exec wraps the command in timeout", () => {
  const inv = buildInvocation(spec({ command: "claude", args: ["--print", "x y"], timeoutMs: 2500, cwd: "/workspace/p" }));
  const c = inv.argv.indexOf(target.container);
  assert.deepEqual(inv.argv.slice(c), [target.container, "timeout", "-k", "10s", "3s", "claude", "--print", "x y"]);
  assert.equal(inv.argv[inv.argv.indexOf("--workdir") + 1], "/workspace/p");
  assert.ok(!inv.argv.includes("-i"));
  assert.ok(buildInvocation(spec({ interactive: true })).argv.includes("-i"));
  const big = buildInvocation(spec({ timeoutMs: 600000 }));
  assert.ok(big.argv.includes("600s"));
  assert.throws(() => buildInvocation(spec({ timeoutMs: 0 })), ExecError);
});

test("S05 admin client rejects unexpected linux users and pod names", () => {
  const ok = parseProject([{ slug: "x", linux_user: "root" }, { slug: "s05", linux_user: "wsm-p-50000" }], "s05");
  assert.deepEqual(ok, { slug: "s05", linuxUser: "wsm-p-50000" });
  assert.equal(parseProject([], "s05"), null);
  for (const user of ["root", "wsm", "wsm-p-", "wsm-p-1/../x", "wsm-p-1 ", "pclip", 50000]) {
    assert.throws(() => parseProject([{ slug: "s05", linux_user: user }], "s05"), AdminError, String(user));
    if (typeof user === "string") assert.throws(() => podmanSocketFor(user), AdminError);
  }
  assert.throws(() => parseProject({ slug: "s05" }, "s05"), AdminError);
  const sb = parseSandbox({ slug: "pc-0123456789ab", status: "running", pod: "sbx-abcdefghij01" });
  assert.equal(sb.container, "sbx-abcdefghij01-work");
  for (const pod of ["sbx-ABCDEFGHIJ01", "sbx-abc", "sbx-abcdefghij01-work", "../sbx-abcdefghij01", "", 1, "x sbx-abcdefghij01"]) {
    assert.throws(() => parseSandbox({ slug: "pc-0123456789ab", status: "running", pod }), AdminError, String(pod));
  }
  assert.throws(() => parseSandbox({ slug: "s1", status: "running", pod: "sbx-abcdefghij01" }), AdminError);
  assert.throws(() => parseSandbox({ slug: "pc-0123456789ab", status: "run ning", pod: "sbx-abcdefghij01" }), AdminError);
});
