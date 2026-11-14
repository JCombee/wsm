# 0011: The MCP server

## Status

Accepted.

## Context

Plan sections 3.3, 3.4 and 3.9 describe an MCP server on `10.255.0.1:7443` with Streamable HTTP, at `/mcp/<sandbox-id>`. It authenticates each sandbox with a bearer token of 32 random bytes, and the database stores only the hash. The token determines the identity, and the path must match it. Section 3.10 requires every call to be audited. CLAUDE.md invariants 2, 4, 6 and 8 apply: identity comes only from the token, the server does not run as root, every call is audited before the response, and everything from a sandbox is untrusted.

Spike 03-mcp (`spikes/03-mcp/FINDINGS.md`, run `20261114T201655Z-ea198fc-tov-450bd5ef`) built a throwaway server with go-sdk v1.8.0. The server ran as `wsm` and was reached from real wsm sandboxes by a probe, Claude Code 2.1.325 and Codex 0.169.0. A scripted mock model in the sandbox supplied the agents' model. All 13 criteria passed in both stateless and stateful mode.

## Decision

1. **go-sdk v1.8.0 joins the main module in M3.** `github.com/modelcontextprotocol/go-sdk` is the official MCP SDK for Go (plan section 2).
   - It implements Streamable HTTP and the protocol versions both clients use: `2025-06-18`, `2025-11-25` and `2026-07-28`.
   - It builds static with `CGO_ENABLED=0`.
   - It adds eight indirect modules: `google/jsonschema-go`, `segmentio/asm`, `segmentio/encoding`, `yosida95/uritemplate/v3`, and `x/oauth2`, `x/sync`, `x/sys` and `x/time`. That is acceptable for a protocol that we would otherwise implement and track ourselves.
   - The version is pinned. An upgrade reruns spike 03's client checks, because the SDK keeps changing stateless behaviour.
2. **Stateless mode** (`StreamableHTTPOptions.Stateless: true`):
   - every request carries the token and is checked on its own, so there is no session state to hijack or lose;
   - a restart of wsm does not break a running agent;
   - Claude Code stays on its sessionless `2026-07-28` protocol, and Codex works unchanged;
   - the same work took 15 requests instead of 23;
   - phase 1 needs no server-to-client requests (sampling, elicitation) and no notifications outside a request, which is all stateless mode gives up;
   - `MCPGODEBUG=allowsessionsinstateless` stays unset.
3. **Tokens and the path:**
   - A token is 32 bytes from `crypto/rand`, encoded as base64url without padding.
   - The database stores `sha256(token)` per sandbox. The plain token exists only when wsm creates it and in the sandbox's environment (`WSM_MCP_TOKEN`, plan 3.4).
   - The verifier hashes the presented token and compares it with `subtle.ConstantTimeCompare`. It returns only `auth.ErrInvalidToken`, because any other error becomes a 500 with the message in the body.
   - After authentication, `/mcp/{sandbox}` must equal the token's sandbox, or the answer is 403.
   - Tools read the identity only from `req.Extra.TokenInfo`. No tool takes a project or sandbox argument.
4. **`auth.RequireBearerToken`:**
   - It runs with `AllowMissingExpiration: true`, because our tokens are opaque and live as long as the sandbox. Without it every request gets 401 "token missing expiration".
   - It runs without `ResourceMetadataURL` and `Scopes`, so a 401 has no `WWW-Authenticate` parameters. Clients then do not start an OAuth flow: Claude Code showed a plain 401 and stopped.
5. **The audit and the limits.**
   - An outer handler wraps the `http.ResponseWriter`. The first `WriteHeader`, `Write` or `Flush` first appends the audit event, with method, path, JSON-RPC method, tool, protocol version, whether a session or Authorization header was present, the sandbox from the token, decision, reason and status. If that append fails, the request gets a 500 instead of its real answer.
   - The wrapper must pass on `Flush` and `Unwrap`, or SSE breaks.
   - Policy decisions (deny outside the ceiling) are made in this outer layer before the tool runs, so they are recorded even though a tool handler cannot reach the HTTP request's context.
   - In M3 the events go through `internal/audit` (`Log.Append`, synchronous, hash-chained, redacted; plan 3.10). They never contain the token or raw headers.
   - The outer handler reads the body once, at most 1 MiB, and `MaxRequestBodyBytes` is also 1 MiB. Parsing the body for the audit is lenient: an unknown shape is recorded as unparsed, and the SDK rejects it.
   - `http.Server` gets `ReadHeaderTimeout` 10 s, `ReadTimeout` 30 s, `WriteTimeout` 10 min, `IdleTimeout` 2 min and `MaxHeaderBytes` 16 KiB.
   - The server runs inside `wsm serve` as user `wsm`, never as root (invariant 4), and listens only on `10.255.0.1:7443`.
6. **The client configuration that wsm puts in a sandbox:**
   - *Claude Code:* a `.mcp.json` with server `wsm`, `"type": "http"`, `"url": "${WSM_MCP_URL}"` and `"headers": {"Authorization": "Bearer ${WSM_MCP_TOKEN}"}`. Claude Code expands the variables itself, so the file holds no secret.
   - *Codex:* `[mcp_servers.wsm]` with the literal URL, `bearer_token_env_var = "WSM_MCP_TOKEN"` and `default_tools_approval_mode = "approve"`. Without the approval mode, `codex exec` skips MCP calls. With `auto`, Codex decides by `readOnlyHint`.
   - Tool annotations are set honestly: `readOnlyHint` for `sandbox_info` and `services_available`, and not for `service_enable`.
   - Codex's own sandbox needs no override for MCP calls.
7. **The mock model is a test tool, not production code.** A scripted mock model for the Anthropic Messages API and the OpenAI Responses API lets isolation tests run real agents without credentials:
   - Claude Code uses it through `ANTHROPIC_BASE_URL` and needs `ENABLE_TOOL_SEARCH=false`;
   - Codex uses it through a custom `model_provider` with `wire_api = "responses"`;
   - Codex offers MCP tools inside a namespace, so the model's `function_call` must carry that `namespace`.

   If M3's isolation tests need it, it lives under `test/`, never under `cmd/` or `internal/`. It never calls a real API.

## Consequences

- M3 adds `internal/mcp`, with the handler chain audit → bearer token → path check → policy → SDK. It also stores a token hash per sandbox, and adds the isolation test "with A's MCP token, do something for sandbox B" (plan section 6).
- Every request costs one synchronous audit append before the answer. With stateless mode that is three to four appends per tool call by Claude Code. This is acceptable for agent-paced traffic.
- Stateless mode rules out server-initiated messages. If a later phase needs them (for example approvals in phase 2), this decision has to be revisited.
- The `mcp_allowed` nftables counter counts connections, not requests. Tests that need per-request evidence use the audit log.
- The SDK's sessionless direction (SEP-2567) may change behaviour in future releases. The pinned version and the rerun on upgrade guard against that.
