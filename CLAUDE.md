# CLAUDE.md

Guidance for Claude Code and other coding agents working in this repository.

## What this is
`wsm` (working name) is a self-hosted manager for isolated development sandboxes for AI coding agents.
- Each project runs under its own Linux user with rootless Podman.
- Agents work inside a sandbox and get least-privilege access to shared services, such as MariaDB, through an MCP server.
- A web dashboard (file browser, Monaco, terminal) and a CLI manage everything.

## Sources of truth
Read these before writing code. They are in Dutch.
- `docs/plan/eisen-workspace-manager.md`: the confirmed requirements.
- `docs/plan/bouwplan-fase-1.md`: the architecture and the phase 1 plan, including scope, repository layout (section 4), spikes, milestones and isolation tests.
- `docs/plan/onderzoek-bestaande-tools.md`: research into existing tools and what we reuse.
- `docs/adr/`: architecture decisions, added as they are made.

The copies in this repository are leading. If the code and these docs disagree, stop and ask the owner instead of silently following either one.

## Communication
- Talk to the owner in Dutch.
- Write code, comments, commit messages and repository docs in English. The exception is `docs/plan/`.

## Current scope: phase 1 only
Build only what `docs/plan/bouwplan-fase-1.md` lists for phase 1. Do not start phase 2 or 3 features unless the owner asks. That includes approvals, notifications, push/re-sign/review, the secret store, TOTP, other engines and auto-stop.

## Order of work
1. **Spikes first.** Each spike lives in `spikes/<nn-name>/`: `01-network`, `02-podman-api`, `03-mcp` and `04-passkeys`.
   - Spike code is throwaway. A spike with Go code gets its own `go.mod`, so it never becomes part of the main module.
   - Each spike ends with a `FINDINGS.md`.
   - Run spikes on a disposable Linux VM, never on a machine with real client data.
   - `01-network` and `02-podman-api` must meet their acceptance criteria before M1 starts. `03-mcp` must pass before M3, and `04-passkeys` before M4.
2. **Then milestones M1 to M5,** in order. Follow the repository layout in section 4 of the plan.
3. **Record every lasting decision** from a spike or milestone as an ADR in `docs/adr/` (`NNNN-short-title.md`: context, decision, consequences).
4. **Grow the isolation tests** in `test/isolation/` with each milestone. A milestone is not done while an isolation test fails.

## Security invariants (non-negotiable)
These rules hold everywhere: in spikes, test helpers and shortcuts too. If a task seems to require breaking one, stop and ask.
1. Never mount the Podman socket or any host path into a sandbox. The only exception is the read-only `wsm-fs` helper at `/opt/wsm/bin`.
2. Sandbox identity comes only from the per-sandbox MCP bearer token. No MCP tool accepts a project or sandbox ID as an argument, and the MCP has no admin tools.
3. The manager never runs git inside a directory an agent controls. Code crosses the sandbox boundary only as git bundles. On mirrors, git runs with hooks and fsmonitor disabled.
4. `wsm-helper` is the only component that runs as root.
   - It accepts only validated IDs and typed rule structs, never paths or shell commands.
   - It serves only the `wsm` user, checked with SO_PEERCRED.
5. Blocks, project config and Containerfiles together form the ceiling.
   - They live in the manager's database and change only through the API (dashboard or CLI).
   - They are never stored where an agent can write.
6. Every MCP call, including denied ones, is written to the audit log before the response is returned.
   - Secrets are redacted.
   - The audit table is append-only and hash-chained.
7. Service credentials are returned only after the matching nftables rule is active. Deleting a sandbox revokes its accounts and drops its prefixed databases.
8. Everything that comes out of a sandbox is untrusted input: exec output, `wsm-fs` JSON, bundles. Enforce size limits and parse strictly.
9. The workspace container always runs:
   - non-root (`--user 1000:1000`, userns keep-id at pod level);
   - with `--cap-drop=ALL`, `no-new-privileges` and the default seccomp profile;
   - with CPU, memory and pids limits.
10. Origins and cookies:
    - Dashboard cookies use the `__Host-` prefix.
    - The passkey RP ID is the dashboard host, with exactly one allowed origin.
    - Previews run on separate origins.
    - The preview proxy strips its own cookie before forwarding to a sandbox.
11. The dashboard never renders sandbox-controlled content as HTML. That covers file previews, Markdown with HTML and SVG. Show it as text, for example in Monaco.

## Engineering conventions
- **Dependencies:** standard library first; keep dependencies minimal. Every new direct dependency gets a one-line justification in the commit. Anything large also gets an ADR.
- **Builds:** `CGO_ENABLED=0`, so binaries stay static. SQLite uses `modernc.org/sqlite`.
- **Code structure:**
  - packages live under `internal/`, binaries under `cmd/`;
  - no global state: pass dependencies explicitly and propagate `context.Context`.
- **Errors:** wrap with context (`fmt.Errorf("...: %w", err)`). Never log secrets.
- **Tests:** table-driven unit tests next to the code. Isolation tests live in `test/isolation/` behind the `isolation` build tag.
- **Formatting and linting:** `gofmt`, `go vet` and `staticcheck` must be clean.
- **Tooling:** M1 adds a `Makefile` with `build`, `test`, `lint` and `isolation-test` targets.
- **Frontend:** lives in `web/`. Vue 3 + TypeScript + Vite is a proposal; confirm with the owner before scaffolding it.
- **Naming:** `wsm` is a working name. Ask the owner for the final name and module path before creating `go.mod`.
