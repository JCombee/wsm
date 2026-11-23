// Spike 05: extracts what e2e.sh needs from Paperclip run logs, without copying
// the raw logs into results/. Usage: node runlog.mjs <log.json>...
// Prints:
//   kv <key>=<value>     KEY=value lines printed by .s05/run.sh (first one wins)
//   pc <line>            "[paperclip] ..." status lines of the run
// The input is the JSON of GET /api/heartbeat-runs/<id>/log: its `content` is
// NDJSON with {stream, chunk}; a chunk may hold Claude Code's stream-json.
import { readFileSync } from "node:fs";

const KEY_RE = /^(scenario|ws-head|issue-get|issue-patch|api-url-host|env-names|env-multiline|bridge-[a-z]+|net-[A-Za-z0-9.:-]+|commit|planted|leak-[a-z]+|s05-end)=(.*)$/;

const kv = new Map();
const pc = [];

function strings(v, out) {
  if (typeof v === "string") out.push(v);
  else if (Array.isArray(v)) for (const x of v) strings(x, out);
  else if (v && typeof v === "object") for (const x of Object.values(v)) strings(x, out);
  return out;
}

function takeLines(text) {
  for (const line of text.split("\n")) {
    const m = KEY_RE.exec(line.trim());
    if (m && !kv.has(m[1])) kv.set(m[1], m[2].trim());
    if (line.startsWith("[paperclip]")) pc.push(line.slice(0, 300));
  }
}

for (const file of process.argv.slice(2)) {
  let doc;
  try {
    doc = JSON.parse(readFileSync(file, "utf8"));
  } catch {
    continue;
  }
  const content = typeof doc.content === "string" ? doc.content : "";
  let text = "";
  for (const line of content.split("\n")) {
    if (!line) continue;
    try {
      const rec = JSON.parse(line);
      if (typeof rec.chunk === "string") text += rec.chunk;
    } catch {}
  }
  for (const line of text.split("\n")) {
    if (line.startsWith("{")) {
      try {
        for (const s of strings(JSON.parse(line), [])) takeLines(s);
        continue;
      } catch {}
    }
    takeLines(line);
  }
}

for (const [k, v] of kv) process.stdout.write(`kv ${k}=${v}\n`);
for (const l of pc) process.stdout.write(`pc ${l}\n`);
