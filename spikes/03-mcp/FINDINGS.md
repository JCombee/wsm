# Findings: spike 03 MCP

- **Run:** 14 November 2026, run `20261114T201655Z-ea198fc-tov-450bd5ef` on commit `ea198fc`. Disposable VM `tov-450bd5ef` (template `debian-13-public`): Debian 13 (trixie), kernel 6.12.115+deb13.1-cloud-amd64, 2 vCPU, 3.9 GiB RAM, Podman 5.4.2. wsm from the same commit, installed with `deploy/install.sh`. In the agent image: node 20.19.2, Claude Code 2.1.325, Codex 0.169.0. Server: go-sdk v1.8.0.
- **Outcome:** **go** for M3. All 13 criteria passed on the first full run. The MCP server runs as `wsm` on `10.255.0.1:7443`. A real wsm sandbox reaches it through the existing `mcp_allowed` rule. Claude Code and Codex make real `tools/call` requests in both stateless and stateful mode, with the identity taken from the token. Every request, including denied ones, was audited before its response, and no token leaked.

## Criteria

Raw output is in `spikes/runs/<run-id>/03-mcp/results/` (gitignored). s1 = `w6ejpxdovliz`, s2 = `64yv7ixaf5mi`, project user `wsm-p-50000`.

| ID | Result | Evidence |
|---|---|---|
| `MCP-LISTEN` | PASS | `ss -ltnp` shows `10.255.0.1:7443`, and `ps -o user=` of that pid is `wsm`. The server is a `systemd-run` transient unit with `--uid=wsm`, `NoNewPrivileges`, `ProtectSystem=strict` and `ProtectHome` (`MCP-LISTEN.log`). |
| `MCP-REACH` | PASS | The probe in s1 with s1's token: `initialize` and `tools/list` return three tools, and `sandbox_info` returns `sandbox w6ejpxdovliz`, in both modes. The nftables counter `mcp_allowed` went from 0 to 2 (`probe-*.txt`). |
| `MCP-NOAUTH` | PASS | No token gives 401. Audit: `auth=none`, `decision=deny`, `status=401`, no sandbox (`probe-audit-*.jsonl`). |
| `MCP-BADTOKEN` | PASS | A random 32-byte token gives 401. Audit: `auth=bearer`, `decision=deny`, `status=401`, no sandbox. |
| `MCP-CROSS` | PASS | s1's token on `/mcp/64yv7ixaf5mi` gives 403. Audit: `sandbox=w6ejpxdovliz`, `path=/mcp/64yv7ixaf5mi`, `reason="path does not match token"`. |
| `MCP-NO-ID-ARGS` | PASS | No input property of any tool contains `project` or `sandbox`. `service_enable` takes only `service`. |
| `MCP-DENY` | PASS | `service_enable` returns `isError: true` with "denied: service_enable is not available in this spike". Audit: `tools/call`, `tool=service_enable`, `decision=deny`, `sandbox=s1`. |
| `MCP-CLAUDE-STATEFUL` | PASS | Claude Code printed `MOCK-RESULT: sandbox w6ejpxdovliz`. The audit has one allowed `tools/call sandbox_info` for s1 (`claude-stateful.txt`). |
| `MCP-CLAUDE-STATELESS` | PASS | Same output, and the same audited call (`claude-stateless.txt`). |
| `MCP-CODEX-STATEFUL` | PASS | Codex printed `MOCK-RESULT: Wall time: 0.0129 seconds Output: sandbox w6ejpxdovliz`, with one audited call (`codex-stateful.txt`). |
| `MCP-CODEX-STATELESS` | PASS | Same, with one audited call (`codex-stateless.txt`). |
| `MCP-AUDIT-COMPLETE` | PASS | The server counts requests on arrival, apart from the audit path. Stateful: 23 seen, 23 audit lines. Stateless: 15 seen, 15 audit lines. |
| `MCP-NO-SECRETS` | PASS | `grep -F -c -f <token file>` over the audit logs, server logs and all of `results/` gives 0. |

## What we learned

**go-sdk v1.8.0 does the job.**
- `mcp.NewStreamableHTTPHandler` and `auth.RequireBearerToken` cover transport and authentication.
- The parts we add ourselves are small: the token verifier, the path check, the audit wrapper and the policy decision.
- `CGO_ENABLED=0` builds a static binary: 14.0 MB unstripped, 9.6 MB with `-s -w`.
- It links these modules: `google/jsonschema-go` v0.4.3, `segmentio/asm` v1.1.3, `segmentio/encoding` v0.5.4, `yosida95/uritemplate/v3` v3.0.2, `x/oauth2` v0.35.0, `x/sync` v0.20.0, `x/sys` v0.41.0 and `x/time` v0.15.0.

**Protocol versions and sessions per client**, from the audit records (`INFO` lines in `summary.txt`):

| Client | Mode | Protocol | Requests |
|---|---|---|---|
| Claude Code 2.1.325 | stateful | `server/discover` on `2026-07-28`, then falls back to `initialize` on `2025-11-25` | discover, initialize, initialized, `tools/list`, `tools/call`, one `GET` (SSE). 4 of 6 carry a session. |
| Claude Code 2.1.325 | stateless | stays on `2026-07-28` | `server/discover`, `subscriptions/listen`, `tools/list`, `tools/call`. No session, no `initialize`. |
| Codex 0.169.0 | stateful | `2025-06-18` | initialize, initialized, `tools/list`, `tools/call`, `GET`, and `DELETE` at the end. 5 of 6 carry a session. |
| Codex 0.169.0 | stateless | `2025-06-18` | initialize, initialized, `tools/list`, `tools/call`. No session ID. |
| go-sdk client (probe) | both | `2025-11-25` | |

Stateless mode needs fewer requests (15 against 23 for the same work). The server's `GET` and `DELETE` answer 405, which neither client minds.

**Client configuration for M3.** The sandbox gets `WSM_MCP_URL` and `WSM_MCP_TOKEN` in its environment (plan 3.4).
- *Claude Code:* `.mcp.json` with `{"mcpServers":{"wsm":{"type":"http","url":"${WSM_MCP_URL}","headers":{"Authorization":"Bearer ${WSM_MCP_TOKEN}"}}}}`. Claude Code expands the variables itself. With a wrong token it gets a 401 and does not try OAuth.
- *Codex:* in `~/.codex/config.toml`, `[mcp_servers.wsm]` with `url = "<WSM_MCP_URL>"`, `bearer_token_env_var = "WSM_MCP_TOKEN"` and `default_tools_approval_mode = "approve"`.
  - The URL is written as a literal. We did not test whether Codex expands variables in `url`.
  - Without the approval mode, `codex exec` skips MCP calls ("interactive approval is unavailable in exec mode"). With `auto`, Codex decides by `readOnlyHint`.
  - Codex ran with its default `sandbox: read-only` and fell back to its bundled bubblewrap. `sandbox_mode="danger-full-access"` was not needed for MCP calls.

**Audit before the response works as a wrapper around the `http.ResponseWriter`.**
- The first `WriteHeader`, `Write` or `Flush` first writes the record, with the status the handler chose.
- If that write fails, the client gets a 500 instead of the real answer.
- A handler that wrote nothing gets its record before `ServeHTTP` returns.
- The wrapper must pass on `Flush` and `Unwrap`, or SSE breaks.
- The policy decision (`service_enable` → deny) is made in that wrapper and recorded before the tool runs. In stateful mode the tool handler's context is not the HTTP request's context, so the tool cannot reliably fill in the audit record itself.

**The token stays out of argv and logs.** It reached sandbox processes only through `podman exec --env-file` with a 0600 file. That file is owned by the project user and removed right after the exec.

**The binary went in over exec stdin, not through a mount.** `podman exec -i <ctr> dd of=/tmp/mcpspike` worked without problems.

## Surprises and open questions

- **`ENABLE_TOOL_SEARCH=false` was needed only because of the mock model.** By default Claude Code hides MCP tools behind its tool-search tool, so `sandbox_info` is not in the tools the model sees. A real model finds the tool through that search. M3 does not need this setting in sandboxes.
- **`mcp_allowed` counts connections, not requests.** It rose by only 2 for a probe that made at least 6 requests, and stood at 11 at the end of the run. The rule probably sits after an established/related accept, so it sees only the first packet of each connection. It is still proof that the traffic went through the wsm egress rules. A test that wants a per-request count must use the audit log.
- **`ldd` exits 1 for a static binary.** Under `pipefail`, the first version of `run.sh` therefore failed its own precondition. Fixed in `ea198fc` before the full run.
- **Claude Code in stateful mode tries `2026-07-28` first.** Against a stateful server it then falls back to `initialize`. That costs one extra request: the audit shows `server/discover` with status 200 (answered by the SDK), followed by `initialize`.
- **go-sdk keeps tightening stateless mode.** Since v1.8.0, stateless mode ignores `Mcp-Session-Id` unless `MCPGODEBUG=allowsessionsinstateless=1` is set. We do not set it. M3 should pin the version and rerun this spike's client checks on every go-sdk upgrade.
- **The audit format is a spike format.** Plan 3.10 wants `audit_events` in SQLite, append-only and hash-chained. M3 must write the same fields through `internal/audit` (`Log.Append`, synchronous), not a JSONL file. This is not a conflict: JSONL was the agreed spike scope.
- **No conflict was found** between plan, code and ADRs.

## Decisions (each one becomes an ADR in docs/adr/)

All in ADR 0011 (`docs/adr/0011-mcp-server.md`):
- go-sdk v1.8.0 becomes a dependency of the main module in M3;
- stateless Streamable HTTP;
- tokens of 32 random bytes, stored as sha256, compared in constant time, plus the path check;
- `AllowMissingExpiration` and no `WWW-Authenticate` parameters;
- an audit wrapper that writes before the response, plus the limits;
- the client configuration for Claude Code and Codex;
- the mock model stays a test tool, not production code.
