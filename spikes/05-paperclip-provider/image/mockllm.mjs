// Scripted stand-in for the Anthropic Messages API, for spike 05 (S1).
// It never calls a real API. A request that offers the Bash tool gets the
// next command from STEPS, chosen by how many tool results the conversation
// already holds. After the last step it answers with text, so the run ends.
import http from "node:http";

const [host, port] = (process.env.MOCK_ADDR ?? "127.0.0.1:8787").split(":");
const MAX_BODY = 32 << 20;

// The scenarios live in the Paperclip project's own repository as .s05/run.sh,
// so a new scenario never needs a new image. The script prints KEY=value lines.
const STEPS = ["sh .s05/run.sh 2>&1"];

function log(msg) {
  process.stderr.write(`${new Date().toISOString()} ${msg}\n`);
}

function countToolResults(req) {
  let n = 0;
  for (const m of req.messages ?? []) {
    if (Array.isArray(m.content)) {
      for (const b of m.content) if (b && b.type === "tool_result") n++;
    }
  }
  return n;
}

function flat(v) {
  if (typeof v === "string") return v;
  if (Array.isArray(v)) return v.map(flat).join(" ");
  if (v && typeof v === "object") return typeof v.text === "string" ? v.text : flat(v.content ?? "");
  return "";
}

function lastToolResult(req) {
  const msgs = req.messages ?? [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const c = msgs[i].content;
    if (Array.isArray(c)) {
      for (const b of c) if (b && b.type === "tool_result") return flat(b.content);
    }
  }
  return "";
}

function userText(req) {
  return (req.messages ?? []).filter((m) => m && m.role === "user").map((m) => flat(m.content)).join(" ");
}

function decide(req) {
  const offersBash = (req.tools ?? []).some((t) => t && t.name === "Bash");
  const done = countToolResults(req);
  // Paperclip's harness test sends "Respond with hello." and checks the answer.
  if (done === 0 && /Respond with hello\./i.test(userText(req))) {
    return { block: { type: "text", text: "hello" }, stop: "end_turn", note: "hello-probe" };
  }
  if (offersBash && done < STEPS.length) {
    return {
      block: { type: "tool_use", id: `toolu_mock_${done + 1}`, name: "Bash", input: { command: STEPS[done], description: `mock step ${done + 1}` } },
      stop: "tool_use",
      note: `step ${done + 1}`,
    };
  }
  const text = done > 0 ? `MOCK-DONE: ${lastToolResult(req).replace(/\s+/g, " ").slice(0, 600)}` : "MOCK-OK";
  return { block: { type: "text", text }, stop: "end_turn", note: done > 0 ? "done" : "plain" };
}

function sse(res, event, data) {
  res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
}

function messages(req, res) {
  const { block, stop, note } = decide(req);
  const usage = { input_tokens: 10, output_tokens: 5 };
  log(`messages stream=${req.stream === true} tools=${(req.tools ?? []).length} -> ${note}`);
  if (req.stream !== true) {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ id: "msg_mock", type: "message", role: "assistant", model: req.model, content: [block], stop_reason: stop, stop_sequence: null, usage }));
    return;
  }
  res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
  sse(res, "message_start", { type: "message_start", message: { id: "msg_mock", type: "message", role: "assistant", model: req.model, content: [], stop_reason: null, stop_sequence: null, usage } });
  if (block.type === "tool_use") {
    sse(res, "content_block_start", { type: "content_block_start", index: 0, content_block: { type: "tool_use", id: block.id, name: block.name, input: {} } });
    sse(res, "content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input) } });
  } else {
    sse(res, "content_block_start", { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } });
    sse(res, "content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: block.text } });
  }
  sse(res, "content_block_stop", { type: "content_block_stop", index: 0 });
  sse(res, "message_delta", { type: "message_delta", delta: { stop_reason: stop, stop_sequence: null }, usage: { output_tokens: 5 } });
  sse(res, "message_stop", { type: "message_stop" });
  res.end();
}

const server = http.createServer((req, res) => {
  const chunks = [];
  let size = 0;
  req.on("data", (c) => {
    size += c.length;
    if (size > MAX_BODY) req.destroy();
    else chunks.push(c);
  });
  req.on("end", () => {
    const path = (req.url ?? "").split("?")[0];
    if (req.method === "POST" && path === "/anthropic/v1/messages/count_tokens") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end('{"input_tokens":10}');
      return;
    }
    if (req.method === "POST" && path === "/anthropic/v1/messages") {
      let body;
      try {
        body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      } catch {
        res.writeHead(400).end("bad request");
        return;
      }
      messages(body, res);
      return;
    }
    log(`unhandled ${req.method} ${path}`);
    res.writeHead(404).end("not found");
  });
});

server.listen(Number(port), host, () => log(`mockllm listening on ${host}:${port}`));
