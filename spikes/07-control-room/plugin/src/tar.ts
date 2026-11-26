// A strict, streaming reader for the tar archives that come out of a sandbox
// (invariant 8: untrusted input). It accepts only what `tar -c --format=pax`
// produces for regular files, directories and inner symlinks, and refuses
// everything else instead of skipping it.
//
// Refused: absolute names, `..`, NUL; hardlinks, devices, fifos, sparse files,
// GNU long names and global pax headers; pax path/linkpath with the same
// faults; symlinks whose target is absolute or leaves the root; a member below
// an earlier symlink; a duplicate member that is not a directory; a truncated
// archive; and anything over the size and count limits. Modes keep only 0o777.
import path from "node:path";

const BLOCK = 512;
const MAX_PATH = 4096;
const MAX_SEGMENT = 255;
const MAX_PAX_HEADER = 1 << 20;

export class TarError extends Error {
  constructor(message: string) {
    super(`s07 archive refused: ${message}`);
  }
}

export interface TarLimits {
  maxTotalBytes: number;
  maxEntryBytes: number;
  maxEntries: number;
}

export const DEFAULT_TAR_LIMITS: TarLimits = {
  maxTotalBytes: 2 * 1024 ** 3,
  maxEntryBytes: 1024 ** 3,
  maxEntries: 200_000,
};

export interface TarSink {
  dir(rel: string, mode: number, mtime: number): Promise<void>;
  file(rel: string, mode: number, mtime: number, size: number, data: AsyncIterable<Buffer>): Promise<void>;
  symlink(rel: string, target: string): Promise<void>;
}

export interface TarStats {
  files: number;
  dirs: number;
  symlinks: number;
  bytes: number;
  archiveBytes: number;
}

/** Blocks pulls exact byte counts out of a chunked async source. */
class Blocks {
  private it: AsyncIterator<Buffer>;
  private buf: Buffer = Buffer.alloc(0);
  private done = false;
  consumed = 0;
  private maxConsumed: number;

  constructor(source: AsyncIterable<Buffer>, maxConsumed: number) {
    this.it = source[Symbol.asyncIterator]();
    this.maxConsumed = maxConsumed;
  }

  private async fill(n: number): Promise<boolean> {
    while (this.buf.length < n && !this.done) {
      const r = await this.it.next();
      if (r.done) {
        this.done = true;
        break;
      }
      const chunk = Buffer.isBuffer(r.value) ? r.value : Buffer.from(r.value);
      this.buf = this.buf.length === 0 ? chunk : Buffer.concat([this.buf, chunk]);
    }
    return this.buf.length >= n;
  }

  /** take returns exactly n bytes, or null at a clean end before any byte. */
  async take(n: number): Promise<Buffer | null> {
    if (!(await this.fill(n))) {
      if (this.buf.length === 0) return null;
      throw new TarError("truncated archive");
    }
    const out = this.buf.subarray(0, n);
    this.buf = this.buf.subarray(n);
    this.consumed += n;
    if (this.consumed > this.maxConsumed) throw new TarError("archive exceeds the size limit");
    return out;
  }

  /** chunks yields up to n bytes in pieces, without buffering them all. */
  async *chunks(n: number): AsyncGenerator<Buffer> {
    let left = n;
    while (left > 0) {
      if (this.buf.length === 0 && !(await this.fill(1))) throw new TarError("truncated archive");
      const take = Math.min(left, this.buf.length, 1 << 16);
      const out = this.buf.subarray(0, take);
      this.buf = this.buf.subarray(take);
      this.consumed += take;
      if (this.consumed > this.maxConsumed) throw new TarError("archive exceeds the size limit");
      left -= take;
      yield out;
    }
  }

  async skip(n: number): Promise<void> {
    for await (const _ of this.chunks(n)) {
      // discard
    }
  }

  /** rest checks that whatever follows is zero padding. */
  async restIsZero(): Promise<boolean> {
    for (;;) {
      if (this.buf.length === 0 && !(await this.fill(1))) return true;
      if (this.buf.some((b) => b !== 0)) return false;
      this.consumed += this.buf.length;
      if (this.consumed > this.maxConsumed) throw new TarError("archive exceeds the size limit");
      this.buf = Buffer.alloc(0);
    }
  }
}

function cstr(b: Buffer, start: number, len: number): string {
  const field = b.subarray(start, start + len);
  const nul = field.indexOf(0);
  return field.subarray(0, nul === -1 ? len : nul).toString("utf8");
}

function octal(b: Buffer, start: number, len: number, what: string): number {
  const field = b.subarray(start, start + len);
  if ((field[0] ?? 0) & 0x80) throw new TarError(`${what} uses base-256 encoding`);
  const s = cstr(b, start, len).trim();
  if (s === "") return 0;
  if (!/^[0-7]+$/.test(s)) throw new TarError(`${what} is not octal`);
  const v = parseInt(s, 8);
  if (!Number.isSafeInteger(v)) throw new TarError(`${what} is out of range`);
  return v;
}

function checksumOk(h: Buffer): boolean {
  const want = octal(h, 148, 8, "checksum");
  let sum = 0;
  for (let i = 0; i < BLOCK; i++) sum += i >= 148 && i < 156 ? 0x20 : (h[i] ?? 0);
  return sum === want;
}

/** cleanName validates a member name and returns it relative to the root ("" for the root). */
export function cleanName(raw: string): string {
  if (raw.length === 0) throw new TarError("empty member name");
  if (raw.length > MAX_PATH) throw new TarError("member name too long");
  if (raw.includes("\0")) throw new TarError("member name holds a NUL byte");
  if (raw.startsWith("/")) throw new TarError(`absolute member name ${JSON.stringify(raw.slice(0, 100))}`);
  let name = raw;
  while (name.startsWith("./")) name = name.slice(2);
  if (name === "." || name === "") return "";
  if (name.endsWith("/")) name = name.slice(0, -1);
  const segs = name.split("/");
  for (const s of segs) {
    if (s === "..") throw new TarError(`member name with .. ${JSON.stringify(raw.slice(0, 100))}`);
    if (s === "" || s === ".") throw new TarError(`member name with an empty or . segment ${JSON.stringify(raw.slice(0, 100))}`);
    if (Buffer.byteLength(s) > MAX_SEGMENT) throw new TarError("member name segment too long");
  }
  return segs.join("/");
}

/**
 * checkLinkTarget refuses symlink targets that are absolute or leave the root.
 *
 * The root check is lexical, so a `..` may only lead the target. The kernel
 * resolves a symlink component before the `..` that follows it: with
 * `a/b/l1 -> ../..` (the root), `a/b/x -> l1/..` normalizes to `a/b` but
 * resolves to the parent of the root. Leading `..` segments climb only through
 * real directories, because no member may sit below a symlink.
 */
export function checkLinkTarget(rel: string, target: string): void {
  if (target.length === 0) throw new TarError(`symlink ${rel} has an empty target`);
  if (target.length > MAX_PATH || target.includes("\0")) throw new TarError(`symlink ${rel} has a bad target`);
  if (target.startsWith("/")) throw new TarError(`symlink ${rel} points to an absolute path`);
  let descended = false;
  for (const seg of target.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (descended) throw new TarError(`symlink ${rel} has .. after a path segment`);
    } else {
      descended = true;
    }
  }
  const resolved = path.posix.normalize(path.posix.join(path.posix.dirname(rel), target));
  if (resolved === ".." || resolved.startsWith("../")) throw new TarError(`symlink ${rel} points out of the root`);
}

function parsePax(data: Buffer): Map<string, string> {
  const out = new Map<string, string>();
  let i = 0;
  while (i < data.length) {
    const sp = data.indexOf(0x20, i);
    if (sp === -1 || sp - i > 10) throw new TarError("malformed pax record");
    const lenStr = data.subarray(i, sp).toString("ascii");
    if (!/^[1-9][0-9]*$/.test(lenStr)) throw new TarError("malformed pax record length");
    const len = Number(lenStr);
    const end = i + len;
    if (end > data.length || data[end - 1] !== 0x0a) throw new TarError("malformed pax record");
    const rec = data.subarray(sp + 1, end - 1);
    const eq = rec.indexOf(0x3d);
    if (eq <= 0) throw new TarError("malformed pax record");
    const key = rec.subarray(0, eq).toString("utf8");
    const value = rec.subarray(eq + 1).toString("utf8");
    if (key.startsWith("GNU.sparse")) throw new TarError("sparse files are not allowed");
    if (value.includes("\0")) throw new TarError(`pax ${key} holds a NUL byte`);
    out.set(key, value);
    i = end;
  }
  return out;
}

/** readTar validates an archive stream and hands its members to the sink. */
export async function readTar(source: AsyncIterable<Buffer>, sink: TarSink, limits: TarLimits = DEFAULT_TAR_LIMITS): Promise<TarStats> {
  const overhead = BLOCK * (3 * limits.maxEntries + 64) + MAX_PAX_HEADER;
  const blocks = new Blocks(source, limits.maxTotalBytes + overhead);
  const stats: TarStats = { files: 0, dirs: 0, symlinks: 0, bytes: 0, archiveBytes: 0 };
  const kinds = new Map<string, "file" | "dir" | "symlink">();
  let pax: Map<string, string> | null = null;
  let entries = 0;

  for (;;) {
    const h = await blocks.take(BLOCK);
    if (h === null) throw new TarError("truncated archive (no end marker)");
    if (h.every((b) => b === 0)) {
      const second = await blocks.take(BLOCK);
      if (second === null || !second.every((b) => b === 0)) throw new TarError("truncated archive (single end block)");
      if (pax) throw new TarError("pax header without a member");
      if (!(await blocks.restIsZero())) throw new TarError("data after the end of the archive");
      stats.archiveBytes = blocks.consumed;
      return stats;
    }
    if (!checksumOk(h)) throw new TarError("bad header checksum");
    if (cstr(h, 257, 6) !== "ustar" || h[262] !== 0 || h.subarray(263, 265).toString("ascii") !== "00") {
      throw new TarError("not a ustar/pax archive");
    }
    const type = String.fromCharCode(h[156] ?? 0);
    let size = octal(h, 124, 12, "size");

    if (type === "x") {
      if (pax) throw new TarError("two pax headers in a row");
      if (size > MAX_PAX_HEADER) throw new TarError("pax header too large");
      const data = await blocks.take(size);
      if (data === null) throw new TarError("truncated archive");
      await blocks.skip((BLOCK - (size % BLOCK)) % BLOCK);
      pax = parsePax(Buffer.from(data));
      continue;
    }
    if (type === "g") throw new TarError("global pax headers are not allowed");
    if (type === "L" || type === "K") throw new TarError("GNU long names are not allowed");
    if (type === "S") throw new TarError("sparse files are not allowed");
    if (type === "1") throw new TarError("hardlinks are not allowed");
    if (type === "3" || type === "4") throw new TarError("device files are not allowed");
    if (type === "6") throw new TarError("fifos are not allowed");
    if (!["0", "\0", "5", "2"].includes(type)) throw new TarError(`member type ${JSON.stringify(type)} is not allowed`);

    const prefix = cstr(h, 345, 155);
    let rawName = prefix ? `${prefix}/${cstr(h, 0, 100)}` : cstr(h, 0, 100);
    let linkName = cstr(h, 157, 100);
    if (pax) {
      const p = pax.get("path");
      if (p !== undefined) rawName = p;
      const l = pax.get("linkpath");
      if (l !== undefined) linkName = l;
      const s = pax.get("size");
      if (s !== undefined) {
        if (!/^[0-9]{1,15}$/.test(s)) throw new TarError("bad pax size");
        size = Number(s);
      }
      pax = null;
    }
    const mtimeRaw = octal(h, 136, 12, "mtime");
    const mode = octal(h, 100, 8, "mode") & 0o777;
    const rel = cleanName(rawName);

    entries++;
    if (entries > limits.maxEntries) throw new TarError(`more than ${limits.maxEntries} members`);
    if (size > limits.maxEntryBytes) throw new TarError(`member larger than ${limits.maxEntryBytes} bytes`);
    if (type !== "0" && type !== "\0" && size !== 0) throw new TarError("non-file member with data");

    // Nothing may be written below a symlink that the archive created.
    if (rel !== "") {
      const segs = rel.split("/");
      for (let i = 1; i < segs.length; i++) {
        const parent = segs.slice(0, i).join("/");
        const k = kinds.get(parent);
        if (k === "symlink") throw new TarError(`member ${rel} is below the symlink ${parent}`);
        if (k === "file") throw new TarError(`member ${rel} is below the file ${parent}`);
      }
    }

    const prev = kinds.get(rel);
    if (type === "5") {
      if (prev !== undefined && prev !== "dir") throw new TarError(`duplicate member ${rel}`);
      if (rel !== "" && prev === undefined) {
        await sink.dir(rel, mode, mtimeRaw);
        stats.dirs++;
      }
      kinds.set(rel, "dir");
      continue;
    }
    if (rel === "") throw new TarError("the root must be a directory");
    if (prev !== undefined) throw new TarError(`duplicate member ${rel}`);

    if (type === "2") {
      checkLinkTarget(rel, linkName);
      kinds.set(rel, "symlink");
      await sink.symlink(rel, linkName);
      stats.symlinks++;
      continue;
    }

    stats.bytes += size;
    if (stats.bytes > limits.maxTotalBytes) throw new TarError(`archive data exceeds ${limits.maxTotalBytes} bytes`);
    kinds.set(rel, "file");
    let fed = 0;
    const data = blocks.chunks(size);
    async function* counted(): AsyncGenerator<Buffer> {
      for await (const c of data) {
        fed += c.length;
        yield c;
      }
    }
    await sink.file(rel, mode, mtimeRaw, size, counted());
    if (fed !== size) throw new TarError(`member ${rel} was not read to the end`);
    await blocks.skip((BLOCK - (size % BLOCK)) % BLOCK);
    stats.files++;
  }
}
