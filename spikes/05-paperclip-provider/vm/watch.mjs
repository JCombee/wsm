// Spike 05 watcher, run as root during a scenario. Every 200 ms it reads all
// /proc/*/cmdline and looks for the sentinel. It reads the sentinel from a file,
// so its own argv never holds it. Hits are recorded as pid and comm only.
// Usage: node watch.mjs <sentinel-file> <out-file>; stop it with SIGTERM.
import { readFileSync, readdirSync, writeFileSync } from "node:fs";

const [sentinelFile, outFile] = process.argv.slice(2);
const sentinel = Buffer.from(readFileSync(sentinelFile, "utf8").trim());
if (sentinel.length < 16) throw new Error("sentinel too short");
const hits = new Map();
let scans = 0;

function scan() {
  scans++;
  for (const name of readdirSync("/proc")) {
    if (!/^[0-9]+$/.test(name)) continue;
    let cmd;
    try {
      cmd = readFileSync(`/proc/${name}/cmdline`);
    } catch {
      continue;
    }
    if (cmd.includes(sentinel)) {
      let comm = "?";
      try {
        comm = readFileSync(`/proc/${name}/comm`, "utf8").trim();
      } catch {}
      hits.set(name, comm);
    }
  }
}

function finish() {
  const lines = [`scans=${scans}`, `hits=${hits.size}`, ...[...hits].map(([pid, comm]) => `hit pid=${pid} comm=${comm}`)];
  writeFileSync(outFile, `${lines.join("\n")}\n`);
  process.exit(0);
}

process.on("SIGTERM", finish);
process.on("SIGINT", finish);
setInterval(scan, 200);
scan();
