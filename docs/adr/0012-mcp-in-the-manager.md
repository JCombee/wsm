# 0012: MCP in the manager

## Status

Accepted.

## Context

ADR 0011 chose the MCP server's design after spike 03-mcp: go-sdk v1.8.0, stateless Streamable HTTP, a bearer token per sandbox, and the handler chain audit → bearer token → path check → policy → SDK. M3a builds that server into `wsm serve` as `internal/mcp`. It gives every new sandbox a token and offers the tools `sandbox_info`, `services_available` and `service_enable`. In M3a, `service_enable` always refuses. This ADR records the lasting choices M3a made on top of ADR 0011.

## Decision

1. **Token format, hash and storage.**
   - A token is 32 bytes from `crypto/rand`, encoded as base64url without padding: 43 characters (`internal/mcptoken`).
   - The stored form is `sha256` over those 43 characters, as lowercase hex, in `sandboxes.mcp_token_hash` (migration `00005`).
     - A `CHECK` constraint allows only NULL or 64 lowercase hex digits.
     - A separate unique index makes the hash unique. SQLite cannot add a `UNIQUE` column, and NULL may occur more than once.
   - `Manager.CreateSandbox` makes the token before the insert. The plain token goes only into the workspace container's spec. It is not in `db.Sandbox`, the API types, logs, jobs or error messages.
   - There is no rotation: a token lives as long as its sandbox. Deleting the sandbox deletes the row, so the token stops working at once. Apart from the row, the verifier does no status check.
2. **Where the token lives.** The token exists only in the workspace container's environment, as `WSM_MCP_TOKEN`, next to `WSM_MCP_URL=http://10.255.0.1:7443/mcp/<sandbox-id>`. The URL is built from the `netpolicy` constants; there is no flag for it. Known exposure:
   - the project user can read the token with `podman container inspect`. That user already controls the sandbox, so this grants nothing new;
   - sandboxes of the same project share `/home/agent`. A token that an agent writes there, for example in a client configuration file, can be read by the project's other sandboxes. Such a token still works only on its own sandbox's path and only for that sandbox's identity. Client configuration should therefore reference the environment variables rather than copy the token (ADR 0011, point 6).
3. **The audit event.** Every HTTP request to the MCP server becomes exactly one event in `internal/audit`, appended before the first byte of the response. If the append fails, the response is a 500 and the real answer is discarded.
   - `Actor` is `mcp:<sandbox-id>` with a valid token, otherwise `mcp:unauthenticated`. `SandboxID` comes from the token, never from the path.
   - `Tool` is one of three things:
     - for `tools/call`, the tool name if it matches `[a-z0-9_]{1,64}`, otherwise `invalid`;
     - for any other JSON-RPC request, `rpc:<method>`. The method must match `[A-Za-z0-9_/]{1,64}`, otherwise the tool is `rpc:invalid`. The brief proposed `[a-z0-9_]`, but MCP methods contain `/` (`tools/list`) and sometimes capitals (`logging/setLevel`);
     - without a JSON-RPC body, `http:<METHOD>`.
   - `Result` follows from what happened:
     - `denied` when one of our layers refused the request: 401, 403, 404, 413, 400 from the body rules, and the policy;
     - `error` for any other status from 400 up, for example the SDK's 405 on GET;
     - `ok` otherwise.
   - `Detail` is a short reason.
   - `Args` holds:
     - `http_method`, `path` (escaped, at most 256 bytes, without control characters) and `status`;
     - `protocol_version`, `mcp_sid_present` and `auth_scheme` (`none`, `bearer`, `basic` or `other`);
     - `rpc_method`;
     - the tool's `arguments`. Above 16 KiB they are replaced by `{"truncated": true, "bytes": N}`, so an append never fails on size.

     The keys avoid words that `internal/audit` redacts (`session`, `authorization`, `token`), so these fields stay readable. The token and raw headers are never recorded. Tool arguments pass through the audit's normal redaction.
   - A 401 records nothing from the body. The bearer check runs before the body is read, on every path, so wsm reads no body from an unknown sender, and the actor of a 403, 404, 413 or 400 is known.
4. **Strict bodies; batches are refused.** This tightens ADR 0011, point 5, which made parsing lenient. A POST must contain exactly one JSON object, at most 1 MiB:
   - a JSON-RPC batch (an array) gets 400;
   - so does any object with duplicate keys, at any depth. Keys that differ only in case count as duplicates (`strings.EqualFold`, implemented as a fold key per rune);
   - so do invalid UTF-8, trailing data, and nesting deeper than 64 levels.

   The policy and the audit then read `method`, `params.name` and `params.arguments` with exact keys, as the SDK does. go-sdk v1.8.0 decodes JSON case-sensitively. It refuses batches only from protocol `2025-06-18` on, and without a protocol header it assumes `2025-03-26` and runs a batch. Under these rules the policy and the SDK always read the same request. Claude Code and Codex send no batches (spike 03).
5. **The policy refuses before the SDK.** A `tools/call` for `service_enable` or for an unknown tool is answered by wsm itself, with a JSON-RPC result that has `isError` and a text starting with `denied:`. The SDK and the tool handler do not run. The `service_enable` handler refuses on its own as well, as a second check. Requests without a JSON-RPC id cannot be answered this way and get 400.
6. **Tools.**
   - `sandbox_info` and `services_available` have `readOnlyHint` and an input schema without properties. They do not forbid extra properties, so a call with stray arguments still works and simply ignores them.
   - `service_enable` has only the input `block` (string), without `readOnlyHint`.
   - The tools read the identity only from `req.Extra.TokenInfo`: the sandbox ID plus the project slug, sandbox slug and branch that the verifier looked up for the token.
   - Until M3b and M5, `sandbox_info` returns an empty `preview_url` and empty `services`, and `services_available` returns an empty list.
7. **Activity.** Every authenticated request on the sandbox's own path updates `last_activity_at`. A failure is logged and does not fail the request.
8. **Listening.** `wsm serve` listens on `tcp4` `10.255.0.1:7443`, after `SetupWSM0` and `ApplyBaseRules`, next to the API socket. A failed bind stops `wsm serve` with an error, and systemd restarts it. `wsm.service` needs no change. There is no other TCP listener, and never one on a wildcard address.
9. **Sandboxes from before migration `00005` get no token** and therefore no MCP access. Whoever needs MCP there recreates the sandbox.
10. **There is no rate limiting** in M3a.

## Open points

- **Client configuration.** M3a only supplies the environment variables. Codex does not expand environment variables in `url`. Because `/home/agent` is shared per project, one `~/.codex/config.toml` with a fixed URL cannot serve every sandbox of a project. One possible solution: also accept `/mcp`, with the identity taken only from the token. That keeps invariant 2, and the path check would then apply only to `/mcp/<id>`.
- **Rate limiting** per token, and per source for unauthenticated requests.
- **Token rotation**, for example when a token leaks into the shared home.

## Consequences

- Every MCP request costs one synchronous audit append, and denied and unauthenticated requests cost one too. An unauthenticated client inside a sandbox can therefore grow the audit log. Rate limiting is the planned answer.
- Clients that send batches or ambiguous JSON are refused. No known client does.
- Code that creates workspace containers must pass the token to `WorkSpec`. A future path that recreates containers needs either the token or a rotation.
- Isolation tests read `audit_events` read-only from the database to see per-request evidence. The `mcp_allowed` counter only counts connections.
