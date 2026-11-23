import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { materializeArchive } from "../src/sync.ts";
import { readTar, TarError, type TarLimits, type TarSink } from "../src/tar.ts";

// ---------------------------------------------------------------------------
// A tiny ustar/pax writer, so every test archive is built byte by byte.
// ---------------------------------------------------------------------------

interface Hdr {
  name: string;
  type?: string;
  size?: number;
  link?: string;
  mode?: number;
  prefix?: string;
  magic?: string;
  version?: string;
}

function header(o: Hdr): Buffer {
  const h = Buffer.alloc(512);
  const type = o.type ?? "0";
  h.write(o.name, 0, 100, "utf8");
  h.write(`${(o.mode ?? (type === "5" ? 0o755 : 0o644)).toString(8).padStart(7, "0")}\0`, 100, 8, "ascii");
  h.write("0001750\0", 108, 8, "ascii");
  h.write("0001750\0", 116, 8, "ascii");
  h.write(`${(o.size ?? 0).toString(8).padStart(11, "0")}\0`, 124, 12, "ascii");
  h.write("15000000000\0", 136, 12, "ascii");
  h.write("        ", 148, 8, "ascii");
  h.write(type, 156, 1, "ascii");
  if (o.link !== undefined) h.write(o.link, 157, 100, "utf8");
  h.write(o.magic ?? "ustar\0", 257, 6, "binary");
  h.write(o.version ?? "00", 263, 2, "binary");
  if (o.prefix !== undefined) h.write(o.prefix, 345, 155, "utf8");
  let sum = 0;
  for (const b of h) sum += b;
  h.write(`${sum.toString(8).padStart(6, "0")}\0 `, 148, 8, "ascii");
  return h;
}

function member(o: Hdr, data: Buffer = Buffer.alloc(0)): Buffer {
  const pad = Buffer.alloc((512 - (data.length % 512)) % 512);
  return Buffer.concat([header({ ...o, size: o.size ?? data.length }), data, pad]);
}

function paxBody(records: Record<string, string>): Buffer {
  let body = "";
  for (const [k, v] of Object.entries(records)) {
    const tail = ` ${k}=${v}\n`;
    let len = Buffer.byteLength(tail) + 1;
    for (;;) {
      const rec = `${len}${tail}`;
      const n = Buffer.byteLength(rec);
      if (n === len) {
        body += rec;
        break;
      }
      len = n;
    }
  }
  return Buffer.from(body, "utf8");
}

function pax(records: Record<string, string>): Buffer {
  return member({ name: "PaxHeaders/0", type: "x" }, paxBody(records));
}

const END = Buffer.alloc(1024);

function archive(...parts: Buffer[]): Buffer {
  return Buffer.concat([...parts, END]);
}

async function* chunked(b: Buffer, size = 700): AsyncGenerator<Buffer> {
  for (let i = 0; i < b.length; i += size) yield b.subarray(i, i + size);
}

function recorder(): { events: string[]; sink: TarSink } {
  const events: string[] = [];
  const sink: TarSink = {
    async dir(rel) {
      events.push(`d ${rel}`);
    },
    async file(rel, mode, _mtime, _size, data) {
      let n = 0;
      for await (const c of data) n += c.length;
      events.push(`f ${rel} ${n} ${mode.toString(8)}`);
    },
    async symlink(rel, target) {
      events.push(`l ${rel} -> ${target}`);
    },
  };
  return { events, sink };
}

async function refused(buf: Buffer, re: RegExp, limits?: TarLimits): Promise<void> {
  const { sink } = recorder();
  await assert.rejects(readTar(chunked(buf), sink, limits), (e: unknown) => {
    assert.ok(e instanceof TarError, `not a TarError: ${String(e)}`);
    assert.match(e.message, re);
    return true;
  });
}

const data = (s: string) => Buffer.from(s, "utf8");

// ---------------------------------------------------------------------------

test("S05 tar accepts files dirs and inner symlinks", async () => {
  const long = `a/${"n".repeat(150)}.txt`;
  const buf = archive(
    member({ name: "./", type: "5" }),
    member({ name: "./a/", type: "5" }),
    member({ name: "./a/x.txt" }, data("hello")),
    member({ name: "./a/ln", type: "2", link: "x.txt" }),
    member({ name: "./a/b/", type: "5" }),
    member({ name: "./a/b/top", type: "2", link: "../../a/x.txt" }),
    pax({ path: long }),
    member({ name: "./a/short-name" }, data("long")),
    member({ name: "file.txt", prefix: "a/b" }, data("p")),
    member({ name: "./a/suid", mode: 0o4755 }, data("s")),
  );
  const want = [
    "d a",
    "f a/x.txt 5 644",
    "l a/ln -> x.txt",
    "d a/b",
    "l a/b/top -> ../../a/x.txt",
    `f ${long} 4 644`,
    "f a/b/file.txt 1 644",
    "f a/suid 1 755",
  ];
  for (const size of [700, 1, 512, 1 << 16]) {
    const { events, sink } = recorder();
    const stats = await readTar(chunked(buf, size), sink);
    assert.deepEqual(events, want, `chunk size ${size}`);
    assert.equal(stats.files, 4);
    assert.equal(stats.dirs, 2);
    assert.equal(stats.symlinks, 2);
    assert.equal(stats.bytes, 11);
  }
});

test("S05 tar rejects dot-dot names", async () => {
  for (const name of ["../x", "./../x", "a/../../x", "a/..", "a/b/../c", ".."]) {
    await refused(archive(member({ name }, data("x"))), /\.\./);
  }
  await refused(archive(member({ name: "..", type: "5" })), /\.\./);
});

test("S05 tar rejects absolute names", async () => {
  for (const name of ["/etc/passwd", "/", "//x"]) {
    await refused(archive(member({ name }, data("x"))), /absolute/);
  }
  await refused(archive(member({ name: "/tmp/", type: "5" })), /absolute/);
});

test("S05 tar rejects escaping symlinks", async () => {
  const dirs = [member({ name: "./a/", type: "5" }), member({ name: "./a/b/", type: "5" })];
  const cases: Array<[Buffer, RegExp]> = [
    [archive(member({ name: "./l", type: "2", link: "/etc/hostname" })), /absolute path/],
    [archive(member({ name: "./l", type: "2", link: "../x" })), /out of the root/],
    [archive(...dirs, member({ name: "./a/b/l", type: "2", link: "../../../etc/hostname" })), /out of the root/],
    [archive(member({ name: "./l", type: "2", link: "" })), /empty target/],
    [archive(member({ name: "./l", type: "2", link: "b/../.." })), /after a path segment/],
    // a/b/l1 points at the root; l1/.. would be the parent of the root.
    [
      archive(
        ...dirs,
        member({ name: "./a/b/l1", type: "2", link: "../.." }),
        member({ name: "./a/b/x", type: "2", link: "l1/../outside" }),
      ),
      /after a path segment/,
    ],
  ];
  for (const [buf, re] of cases) await refused(buf, re);
});

test("S05 tar rejects hardlinks devices and fifos", async () => {
  const cases: Array<[string, RegExp]> = [
    ["1", /hardlinks/],
    ["3", /device/],
    ["4", /device/],
    ["6", /fifos/],
    ["7", /member type/],
    ["S", /sparse/],
    ["L", /GNU long names/],
    ["K", /GNU long names/],
    ["g", /global pax/],
  ];
  for (const [type, re] of cases) {
    await refused(archive(member({ name: "./x", type, link: type === "1" ? "y" : undefined })), re);
  }
});

test("S05 tar rejects pax path overrides", async () => {
  const sym = (link: string) => member({ name: "./l", type: "2", link });
  const file = member({ name: "./ok" }, data("x"));
  const cases: Array<[Buffer, RegExp]> = [
    [archive(pax({ path: "../evil" }), file), /\.\./],
    [archive(pax({ path: "a/../../evil" }), file), /\.\./],
    [archive(pax({ path: "/abs/evil" }), file), /absolute/],
    [archive(pax({ linkpath: "/etc/hostname" }), sym("ok")), /absolute path/],
    [archive(pax({ linkpath: "../../x" }), sym("ok")), /out of the root/],
    [archive(pax({ "GNU.sparse.map": "0,1" }), file), /sparse/],
    [archive(pax({ path: "a\u0000b" }), file), /NUL/],
    [archive(member({ name: "PaxHeaders/0", type: "x" }, data("abc")), file), /malformed pax/],
    [archive(pax({ path: "x" })), /pax header without a member/],
    [archive(pax({ path: "x" }), pax({ path: "y" }), file), /two pax headers/],
    [archive(pax({ size: "-1" }), file), /bad pax size/],
  ];
  for (const [buf, re] of cases) await refused(buf, re);
});

test("S05 tar rejects writes through a symlink", async () => {
  const cases: Array<[Buffer, RegExp]> = [
    [
      archive(member({ name: "./b/", type: "5" }), member({ name: "./a", type: "2", link: "b" }), member({ name: "./a/x" }, data("x"))),
      /below the symlink a/,
    ],
    [archive(member({ name: "./l", type: "2", link: "." }), member({ name: "./l/x" }, data("x"))), /below the symlink l/],
    [archive(member({ name: "./f" }, data("f")), member({ name: "./f/x" }, data("x"))), /below the file f/],
    [archive(member({ name: "./x" }, data("1")), member({ name: "./x" }, data("2"))), /duplicate member x/],
    [archive(member({ name: "./s", type: "2", link: "d" }), member({ name: "./s/", type: "5" })), /duplicate member s/],
    [archive(member({ name: "./d/", type: "5" }), member({ name: "./d" }, data("x"))), /duplicate member d/],
  ];
  for (const [buf, re] of cases) await refused(buf, re);
});

test("S05 tar enforces size and count limits", async () => {
  const limits: TarLimits = { maxTotalBytes: 1000, maxEntryBytes: 600, maxEntries: 3 };
  await refused(archive(member({ name: "./big" }, Buffer.alloc(700, 1))), /larger than 600/, limits);
  await refused(
    archive(...["a", "b", "c", "d"].map((n) => member({ name: `./${n}` }, data(n)))),
    /more than 3 members/,
    limits,
  );
  await refused(
    archive(member({ name: "./a" }, Buffer.alloc(600, 1)), member({ name: "./b" }, Buffer.alloc(600, 2))),
    /exceeds 1000 bytes/,
    limits,
  );
  // A pax header larger than 1 MiB is refused before its data is read.
  await refused(archive(header({ name: "PaxHeaders/0", type: "x", size: 2 << 20 })), /pax header too large/);
  // Within the limits the same shapes pass.
  const { sink } = recorder();
  const ok = archive(...["a", "b", "c"].map((n) => member({ name: `./${n}` }, Buffer.alloc(300, 1))));
  const stats = await readTar(chunked(ok), sink, limits);
  assert.equal(stats.files, 3);
});

test("S05 tar rejects truncated archives", async () => {
  const file = member({ name: "./x" }, Buffer.alloc(1000, 7));
  const badSum = header({ name: "./x" });
  badSum.write("y", 2, 1, "ascii");
  const cases: Array<[Buffer, RegExp]> = [
    [file, /no end marker/],
    [file.subarray(0, 512 + 300), /truncated archive/],
    [file.subarray(0, 200), /truncated archive/],
    [Buffer.concat([file, Buffer.alloc(512)]), /single end block/],
    [archive(badSum), /bad header checksum/],
    [archive(member({ name: "./x", magic: "\0\0\0\0\0\0", version: "\0\0" }, data("x"))), /not a ustar\/pax archive/],
    [archive(member({ name: "./x", magic: "ustar ", version: " \0" }, data("x"))), /not a ustar\/pax archive/],
    [Buffer.concat([archive(member({ name: "./x" }, data("x"))), data("garbage")]), /data after the end/],
    [Buffer.alloc(0), /no end marker/],
  ];
  for (const [buf, re] of cases) await refused(buf, re);
});

test("S05 sync out writes only inside the staging root", async () => {
  const base = await fs.mkdtemp(path.join(os.tmpdir(), "s05-sync-"));
  try {
    const target = path.join(base, "ws");
    await fs.mkdir(target);
    await fs.writeFile(path.join(target, "old.txt"), "old");
    await fs.writeFile(path.join(base, "outside.txt"), "outside");

    // A valid archive replaces the target as a whole.
    const good = archive(
      member({ name: "./", type: "5" }),
      member({ name: "./new.txt" }, data("new")),
      member({ name: "./d/", type: "5" }),
      member({ name: "./d/in", type: "2", link: "../new.txt" }),
    );
    const stats = await materializeArchive(chunked(good), target);
    assert.equal(stats.files, 1);
    assert.deepEqual((await fs.readdir(target)).sort(), ["d", "new.txt"]);
    assert.equal(await fs.readlink(path.join(target, "d", "in")), "../new.txt");
    assert.equal(await fs.readFile(path.join(target, "d", "in"), "utf8"), "new");

    // Hostile archives are refused: the target stays as it was, nothing lands
    // outside it, and no staging directory is left behind.
    const hostile = [
      archive(member({ name: "./evil", type: "2", link: "../outside.txt" })),
      archive(
        member({ name: "./a/", type: "5" }),
        member({ name: "./a/b/", type: "5" }),
        member({ name: "./a/b/l1", type: "2", link: "../.." }),
        member({ name: "./a/b/x", type: "2", link: "l1/../outside.txt" }),
      ),
      archive(member({ name: "./ln", type: "2", link: "." }), member({ name: "./ln/x" }, data("x"))),
      archive(member({ name: "../outside.txt" }, data("pwned"))),
      archive(member({ name: "./x" }, data("half")), member({ name: "/etc/x" }, data("x"))),
    ];
    for (const h of hostile) await assert.rejects(materializeArchive(chunked(h), target), TarError);
    assert.deepEqual((await fs.readdir(base)).sort(), ["outside.txt", "ws"]);
    assert.equal(await fs.readFile(path.join(base, "outside.txt"), "utf8"), "outside");
    assert.deepEqual((await fs.readdir(target)).sort(), ["d", "new.txt"]);
    assert.equal(await fs.readFile(path.join(target, "new.txt"), "utf8"), "new");
  } finally {
    await fs.rm(base, { recursive: true, force: true });
  }
});
