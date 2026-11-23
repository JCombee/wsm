// Native file sync over exec (doc/plugins/SANDBOX_FILE_SYNC_HOOKS.md).
//
// In:  a file goes over stdin to a temporary name in the target's directory,
//      then chmod and mv. A directory goes as a host `tar` stream into
//      `tar -x --no-same-owner` in the sandbox. Paths travel as positional
//      arguments, never inside the script text.
// Out: the sandbox runs `tar -c --format=pax`; the plugin reads the stream with
//      its own strict reader (tar.ts) and writes into a fresh staging
//      directory next to the target, then swaps it in. No host `tar` reads
//      sandbox output.
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { constants as FS, createReadStream, promises as fs } from "node:fs";
import path from "node:path";
import { PassThrough, type Readable } from "node:stream";
import { ExecError, runExec, runStreaming, type ExecLog, type ExecResult, type ExecTarget } from "./exec.ts";
import { checkLinkTarget, DEFAULT_TAR_LIMITS, readTar, TarError, type TarLimits, type TarSink, type TarStats } from "./tar.ts";

export const HOST_TAR = "/usr/bin/tar";

export interface SyncMapping {
  sourcePath: string;
  targetPath: string;
  kind: "file" | "directory";
  mode?: number;
  exclude?: string[];
  followSymlinks?: boolean;
  preserveSourceEntries?: boolean;
  writablePath?: string;
}

export interface SyncCommand {
  command: string;
  cwd?: string;
  timeoutMs?: number;
}

export interface SyncOperation {
  operationId: string;
  files: SyncMapping[];
  postUploadCommands?: SyncCommand[];
}

export interface SyncOpResult {
  operationId: string;
  filesTransferred: number;
  bytesTransferred: number;
}

export interface SyncContext {
  target: ExecTarget;
  timeoutMs: number;
  log: ExecLog;
  limits?: TarLimits;
}

export class SyncError extends Error {}

function absPath(p: unknown, what: string): string {
  if (typeof p !== "string" || p.length === 0 || p.length > 4096 || p.includes("\0")) throw new SyncError(`${what} is not a valid path`);
  if (!path.posix.isAbsolute(p) || path.posix.normalize(p) !== p.replace(/\/+$/, "") && path.posix.normalize(p) !== p) {
    throw new SyncError(`${what} must be an absolute, normalized path`);
  }
  if (p.split("/").includes("..")) throw new SyncError(`${what} must not contain ..`);
  return p;
}

function within(p: string, root: string): boolean {
  return p === root || p.startsWith(root.endsWith("/") ? root : `${root}/`);
}

function mode8(m: number): string {
  return (m & 0o777).toString(8).padStart(3, "0");
}

// ---------------------------------------------------------------------------
// Host side: the filesystem sink and the staging swap.
// ---------------------------------------------------------------------------

interface FsSink extends TarSink {
  finish(): Promise<void>;
}

/** fsSink writes validated members under root and nowhere else. */
export function fsSink(root: string): FsSink {
  const dirs: Array<{ full: string; mode: number; mtime: number; depth: number }> = [];
  const links: Array<{ rel: string; full: string }> = [];

  async function ensureParents(rel: string): Promise<void> {
    const segs = rel.split("/").slice(0, -1);
    let cur = root;
    for (const s of segs) {
      cur = path.join(cur, s);
      let st;
      try {
        st = await fs.lstat(cur);
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code !== "ENOENT") throw e;
        await fs.mkdir(cur, { mode: 0o700 });
        continue;
      }
      if (!st.isDirectory() || st.isSymbolicLink()) throw new TarError(`parent of ${rel} is not a directory`);
    }
  }

  return {
    async dir(rel, mode, mtime) {
      await ensureParents(rel);
      const full = path.join(root, rel);
      try {
        await fs.mkdir(full, { mode: 0o700 });
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code !== "EEXIST") throw e;
        const st = await fs.lstat(full);
        if (!st.isDirectory() || st.isSymbolicLink()) throw new TarError(`${rel} exists and is not a directory`);
      }
      dirs.push({ full, mode, mtime, depth: rel.split("/").length });
    },
    async file(rel, mode, mtime, size, data) {
      await ensureParents(rel);
      const full = path.join(root, rel);
      const fh = await fs.open(full, FS.O_WRONLY | FS.O_CREAT | FS.O_EXCL | FS.O_NOFOLLOW, 0o600);
      let written = 0;
      try {
        for await (const chunk of data) {
          await fh.write(chunk);
          written += chunk.length;
        }
        if (written !== size) throw new TarError(`member ${rel} size mismatch`);
        await fh.chmod(mode & 0o777);
        await fh.utimes(mtime, mtime);
      } finally {
        await fh.close();
      }
    },
    async symlink(rel, target) {
      checkLinkTarget(rel, target);
      await ensureParents(rel);
      const full = path.join(root, rel);
      await fs.symlink(target, full);
      links.push({ rel, full });
    },
    async finish() {
      // Defense in depth: a link whose target exists must resolve inside root.
      const realRoot = await fs.realpath(root);
      for (const l of links) {
        let real: string;
        try {
          real = await fs.realpath(l.full);
        } catch {
          continue;
        }
        if (!within(real, realRoot)) throw new TarError(`symlink ${l.rel} resolves out of the root`);
      }
      dirs.sort((a, b) => b.depth - a.depth);
      for (const d of dirs) {
        await fs.chmod(d.full, d.mode & 0o777);
        await fs.utimes(d.full, d.mtime, d.mtime);
      }
    },
  };
}

/**
 * stageArchive reads an archive into a fresh staging directory next to
 * targetDir. On any error the staging directory is removed.
 */
export async function stageArchive(source: AsyncIterable<Buffer>, targetDir: string, limits: TarLimits = DEFAULT_TAR_LIMITS): Promise<{ staging: string; stats: TarStats }> {
  const parent = path.dirname(targetDir);
  const staging = await fs.mkdtemp(path.join(parent, `.${path.basename(targetDir)}.s05-staging-`));
  try {
    const sink = fsSink(staging);
    const stats = await readTar(source, sink, limits);
    await sink.finish();
    return { staging, stats };
  } catch (e) {
    await fs.rm(staging, { recursive: true, force: true }).catch(() => {});
    throw e;
  }
}

/** swapIn replaces targetDir with a staged tree; on failure targetDir is restored. */
export async function swapIn(staging: string, targetDir: string): Promise<void> {
  let old: string | null = null;
  try {
    let rootMode = 0o700;
    try {
      const st = await fs.lstat(targetDir);
      if (st.isSymbolicLink() || !st.isDirectory()) throw new SyncError("sync target is not a directory");
      rootMode = st.mode & 0o777;
      old = `${targetDir}.s05-old-${randomBytes(4).toString("hex")}`;
      await fs.rename(targetDir, old);
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "ENOENT") throw e;
      old = null;
    }
    await fs.chmod(staging, rootMode);
    await fs.rename(staging, targetDir);
  } catch (e) {
    if (old) await fs.rename(old, targetDir).catch(() => {});
    await fs.rm(staging, { recursive: true, force: true }).catch(() => {});
    throw e;
  }
  if (old) await fs.rm(old, { recursive: true, force: true });
}

/**
 * materializeArchive reads an archive into a fresh staging directory next to
 * targetDir, and only when the whole archive is valid swaps it in for
 * targetDir. On any error targetDir is left untouched and staging is removed.
 */
export async function materializeArchive(source: AsyncIterable<Buffer>, targetDir: string, limits: TarLimits = DEFAULT_TAR_LIMITS): Promise<TarStats> {
  const { staging, stats } = await stageArchive(source, targetDir, limits);
  await swapIn(staging, targetDir);
  return stats;
}

/** stageHostFile streams one file into a same-directory temp name that already has the final mode. */
async function stageHostFile(source: AsyncIterable<Buffer>, target: string, mode: number, maxBytes: number): Promise<{ tmp: string; bytes: number }> {
  const tmp = path.join(path.dirname(target), `.${path.basename(target)}.s05-${randomBytes(4).toString("hex")}`);
  const fh = await fs.open(tmp, FS.O_WRONLY | FS.O_CREAT | FS.O_EXCL | FS.O_NOFOLLOW, mode & 0o777);
  let n = 0;
  try {
    try {
      await fh.chmod(mode & 0o777);
      for await (const c of source) {
        n += c.length;
        if (n > maxBytes) throw new SyncError(`file exceeds ${maxBytes} bytes`);
        await fh.write(c);
      }
    } finally {
      await fh.close();
    }
    return { tmp, bytes: n };
  } catch (e) {
    await fs.rm(tmp, { force: true }).catch(() => {});
    throw e;
  }
}

// ---------------------------------------------------------------------------
// Sandbox side.
// ---------------------------------------------------------------------------

const FILE_IN_SCRIPT =
  'set -e; d=$(dirname -- "$1"); mkdir -p -- "$d"; t="$d/.s05-upload-$$-$(basename -- "$1")"; ' +
  '(umask 077; cat >"$t"); chmod "$2" "$t"; mv -f -- "$t" "$1"';
const DIR_IN_SCRIPT = 'set -e; rm -rf -- "$1"; mkdir -p -- "$1"; tar -x --no-same-owner -p -f - -C "$1"';

async function countHostTree(root: string): Promise<{ files: number; bytes: number }> {
  let files = 0;
  let bytes = 0;
  const walk = async (dir: string): Promise<void> => {
    for (const e of await fs.readdir(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) await walk(p);
      else if (e.isFile()) {
        files++;
        bytes += (await fs.stat(p)).size;
      }
    }
  };
  await walk(root);
  return { files, bytes };
}

async function syncInFile(ctx: SyncContext, m: SyncMapping): Promise<{ files: number; bytes: number }> {
  const src = absPath(m.sourcePath, "sourcePath");
  const dst = absPath(m.targetPath, "targetPath");
  const st = await fs.stat(src);
  if (!st.isFile()) throw new SyncError("sync-in source is not a regular file");
  const mode = m.mode ?? st.mode & 0o777;
  const r = await runExec(
    { target: ctx.target, command: "sh", args: ["-c", FILE_IN_SCRIPT, "sh", dst, mode8(mode)], cwd: "/", timeoutMs: ctx.timeoutMs, interactive: true },
    { stdin: createReadStream(src), log: ctx.log, outputLimit: 1 << 20 },
  );
  if (r.exitCode !== 0) throw new SyncError(`sync-in of a file failed (exit ${r.exitCode}${r.timedOut ? ", timed out" : ""}): ${r.stderr.toString("utf8").slice(0, 300)}`);
  return { files: 1, bytes: st.size };
}

async function syncInDir(ctx: SyncContext, m: SyncMapping): Promise<{ files: number; bytes: number }> {
  const src = absPath(m.sourcePath, "sourcePath");
  const dst = absPath(m.targetPath, "targetPath");
  const excludes = ["._*", ...(m.exclude ?? [])].map((e) => `--exclude=${e}`);
  const hostTar = spawn(HOST_TAR, ["-c", "-f", "-", "-C", src, ...excludes, ...(m.followSymlinks ? ["-h"] : []), "."], {
    cwd: "/",
    env: { PATH: "/usr/bin:/bin", LANG: "C.UTF-8" },
    shell: false,
    stdio: ["ignore", "pipe", "pipe"],
  });
  let hostErr = "";
  hostTar.stderr?.on("data", (c: Buffer) => {
    if (hostErr.length < 2000) hostErr += c.toString("utf8");
  });
  const hostDone = new Promise<number | null>((resolve) => hostTar.on("close", (code) => resolve(code)));
  let bytes = 0;
  const counter = new PassThrough();
  counter.on("data", (c: Buffer) => (bytes += c.length));
  hostTar.stdout?.pipe(counter);
  const r = await runExec(
    { target: ctx.target, command: "sh", args: ["-c", DIR_IN_SCRIPT, "sh", dst], cwd: "/", timeoutMs: ctx.timeoutMs, interactive: true },
    { stdin: counter as Readable, log: ctx.log, outputLimit: 1 << 20 },
  );
  const hostCode = await hostDone;
  if (hostCode !== 0) throw new SyncError(`host tar failed (exit ${hostCode}): ${hostErr.slice(0, 300)}`);
  if (r.exitCode !== 0) throw new SyncError(`sync-in of a directory failed (exit ${r.exitCode}): ${r.stderr.toString("utf8").slice(0, 300)}`);
  const counted = await countHostTree(src).catch(() => ({ files: 0, bytes: 0 }));
  return { files: counted.files, bytes };
}

function commandRoots(op: SyncOperation): string[] {
  const roots: string[] = [];
  for (const f of op.files) {
    roots.push(path.posix.dirname(f.targetPath));
    if (f.kind === "directory") roots.push(f.targetPath);
    if (f.writablePath) roots.push(f.writablePath);
  }
  return roots;
}

async function runPostUpload(ctx: SyncContext, op: SyncOperation): Promise<void> {
  const roots = commandRoots(op);
  for (const [i, c] of (op.postUploadCommands ?? []).entries()) {
    if (typeof c.command !== "string" || c.command.length === 0 || c.command.includes("\0")) throw new SyncError("invalid post-upload command");
    const cwd = c.cwd === undefined ? roots[0] : absPath(c.cwd, "post-upload cwd");
    if (!cwd || !roots.some((r) => within(cwd, r))) throw new SyncError("post-upload cwd is outside the operation's target roots");
    const timeoutMs = typeof c.timeoutMs === "number" && c.timeoutMs > 0 ? c.timeoutMs : ctx.timeoutMs;
    // The command is opaque and Paperclip-authored: run it verbatim.
    const r = await runExec({ target: ctx.target, command: "sh", args: ["-c", c.command], cwd, timeoutMs }, { log: ctx.log, outputLimit: 4 << 20 });
    if (r.exitCode !== 0) {
      throw new SyncError(`post-upload command ${i + 1} failed (exit ${r.exitCode}${r.timedOut ? ", timed out" : ""}): ${r.stderr.toString("utf8").slice(0, 300)}`);
    }
  }
}

export async function syncIn(ctx: SyncContext, operations: SyncOperation[]): Promise<SyncOpResult[]> {
  const out: SyncOpResult[] = [];
  for (const op of operations) {
    let files = 0;
    let bytes = 0;
    for (const m of op.files) {
      const r = m.kind === "file" ? await syncInFile(ctx, m) : m.kind === "directory" ? await syncInDir(ctx, m) : null;
      if (!r) throw new SyncError("unknown sync mapping kind");
      files += r.files;
      bytes += r.bytes;
    }
    await runPostUpload(ctx, op);
    ctx.log.info(`s05 sync in op=${op.operationId.slice(0, 40)} mappings=${op.files.length} commands=${op.postUploadCommands?.length ?? 0} files=${files} bytes=${bytes}`);
    out.push({ operationId: op.operationId, filesTransferred: files, bytesTransferred: bytes });
  }
  return out;
}

async function syncOutDir(ctx: SyncContext, m: SyncMapping): Promise<{ files: number; bytes: number }> {
  const src = absPath(m.sourcePath, "sourcePath");
  const dst = absPath(m.targetPath, "targetPath");
  const excludes = m.preserveSourceEntries
    ? ["--anchored", "--no-wildcards", ...(m.exclude ?? []).map((e) => `--exclude=./${e}`)]
    : ["._*", ...(m.exclude ?? [])].map((e) => `--exclude=${e}`);
  const run = runStreaming(
    { target: ctx.target, command: "tar", args: ["-c", "--format=pax", "-f", "-", "-C", src, ...excludes, ...(m.followSymlinks ? ["-h"] : []), "."], cwd: "/", timeoutMs: ctx.timeoutMs },
    { log: ctx.log },
  );
  let staged: { staging: string; stats: TarStats };
  try {
    staged = await stageArchive(run.stdout as AsyncIterable<Buffer>, dst, ctx.limits ?? DEFAULT_TAR_LIMITS);
  } catch (e) {
    run.kill();
    await run.done.catch(() => null);
    throw e;
  }
  // The host target changes only after the sandbox tar exited cleanly.
  const dropStaging = () => fs.rm(staged.staging, { recursive: true, force: true }).catch(() => {});
  let r: ExecResult;
  try {
    r = await run.done;
  } catch (e) {
    await dropStaging();
    throw e;
  }
  if (r.exitCode !== 0) {
    await dropStaging();
    throw new SyncError(`sandbox tar failed (exit ${r.exitCode}${r.timedOut ? ", timed out" : ""}): ${r.stderr.toString("utf8").slice(0, 300)}`);
  }
  await swapIn(staged.staging, dst);
  return { files: staged.stats.files, bytes: staged.stats.bytes };
}

async function syncOutFile(ctx: SyncContext, m: SyncMapping): Promise<{ files: number; bytes: number }> {
  const src = absPath(m.sourcePath, "sourcePath");
  const dst = absPath(m.targetPath, "targetPath");
  const run = runStreaming({ target: ctx.target, command: "cat", args: ["--", src], cwd: "/", timeoutMs: ctx.timeoutMs }, { log: ctx.log });
  let staged: { tmp: string; bytes: number };
  try {
    staged = await stageHostFile(run.stdout as AsyncIterable<Buffer>, dst, m.mode ?? 0o600, (ctx.limits ?? DEFAULT_TAR_LIMITS).maxEntryBytes);
  } catch (e) {
    run.kill();
    await run.done.catch(() => null);
    throw e;
  }
  // The host target changes only after the sandbox cat exited cleanly.
  const dropTmp = () => fs.rm(staged.tmp, { force: true }).catch(() => {});
  let r: ExecResult;
  try {
    r = await run.done;
  } catch (e) {
    await dropTmp();
    throw e;
  }
  if (r.exitCode !== 0) {
    await dropTmp();
    throw new SyncError(`sandbox cat failed (exit ${r.exitCode}): ${r.stderr.toString("utf8").slice(0, 300)}`);
  }
  await fs.rename(staged.tmp, dst);
  return { files: 1, bytes: staged.bytes };
}

export async function syncOut(ctx: SyncContext, operations: SyncOperation[]): Promise<SyncOpResult[]> {
  const out: SyncOpResult[] = [];
  for (const op of operations) {
    let files = 0;
    let bytes = 0;
    for (const m of op.files) {
      const r = m.kind === "file" ? await syncOutFile(ctx, m) : m.kind === "directory" ? await syncOutDir(ctx, m) : null;
      if (!r) throw new SyncError("unknown sync mapping kind");
      files += r.files;
      bytes += r.bytes;
    }
    if (op.postUploadCommands?.length) throw new SyncError("post-upload commands are not supported on sync-out");
    ctx.log.info(`s05 sync out op=${op.operationId.slice(0, 40)} mappings=${op.files.length} files=${files} bytes=${bytes}`);
    out.push({ operationId: op.operationId, filesTransferred: files, bytesTransferred: bytes });
  }
  return out;
}

export { ExecError };
