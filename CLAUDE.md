# CLAUDE.md

Guidance for Claude Code and other coding agents working in this repository.

## What this is
`wsm` (working name) is a self-hosted manager for isolated development sandboxes for AI coding agents.
- Each project runs under its own Linux user with rootless Podman.
- Agents work inside a sandbox and get least-privilege access to shared services, such as MariaDB, through an MCP server.
- A web dashboard (file browser, Monaco, terminal) and a CLI manage everything.
- Paperclip runs on top as the control plane: one control room per project, which drives that project's sandboxes through wsm's tenant API.

## Sources of truth
Read these before writing code. They are in Dutch.
- `docs/plan/eisen-workspace-manager.md`: the confirmed requirements.
- `docs/plan/bouwplan-fase-1.md`: the architecture and the phase 1 plan, including scope, repository layout (section 4), spikes, milestones and isolation tests.
- `docs/plan/plan-paperclip.md`: the accepted plan for running wsm under Paperclip (24 November 2026). Its decisions are in §13.1 and §13.6.
- `docs/plan/onderzoek-bestaande-tools.md`: research into existing tools and what we reuse.
- `docs/adr/`: architecture decisions, added as they are made.

The copies in this repository are leading. If the code and these docs disagree, stop and ask the owner instead of silently following either one.

## Communication
- Talk to the owner in Dutch.
- Write code, comments, commit messages and repository docs in English. The exception is `docs/plan/`.

## Current scope: the Paperclip plan
Phase 1 (`docs/plan/bouwplan-fase-1.md`) is done. Now build what `docs/plan/plan-paperclip.md` §13 lists: the spikes S2 to S5, then milestones P1 to P6.
- The plan pulls in these phase 2 parts:
  - push requests with review and re-signing;
  - step-up;
  - approvals for pushes and for requests outside the ceiling;
  - a maximum number of sandboxes in the ceiling.
- Paperclip's secrets and AI connections replace the planned secret store and the per-project harness home (B9, B10).
- Everything else from phases 2 and 3 stays out unless the owner asks. That includes notifications, TOTP, other service engines, auto-stop, the status, reset and inspection tools, and the shared Containerfile library.

## Order of work
1. **Spikes first.** Each spike lives in `spikes/<nn-name>/`.
   - Phase 1 had `01-network`, `02-podman-api`, `03-mcp` and `04-passkeys`. The Paperclip plan started with `05-paperclip-provider` (S1).
   - The next Paperclip spikes continue the numbering, in the order S3, S2, S4, S5 (plan §13.6).
   - Spike code is throwaway. A spike with Go code gets its own `go.mod`, so it never becomes part of the main module.
   - Each spike ends with a `FINDINGS.md`.
   - Run spikes on a disposable Linux VM, never on a machine with real client data.
   - `01-network` and `02-podman-api` had to meet their acceptance criteria before M1 started, `03-mcp` before M3 and `04-passkeys` before M4. A Paperclip milestone starts only after the spikes it builds on have passed.
2. **Then the milestones, in order:** M1 to M5 for phase 1 (done), then P1 to P6 (plan-paperclip §13.3). Follow the repository layout in section 4 of the phase 1 plan.
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
    - The Paperclip board is not the dashboard. It may show agent HTML, but only in Paperclip's sandboxed iframe (plan E4, decision B7).

The Paperclip plan (§5.4) adds five more:

12. A control room never runs project code. It runs no local adapters, no workspace services and no stdio MCP.
13. A sandbox has no network path to a control room. All traffic between them goes through wsm's tenant API, as exec or file transfer.
14. A control room reaches only its own project's sandboxes, and only within the ceiling.
    - Its identity comes from the connection (SO_PEERCRED on its tenant socket), never from an argument.
    - It gets no Podman socket and no host path. Only its own project's tenant socket is mounted.
15. Git credentials and signing keys live only in the wsm core, never in a control room or a sandbox.
16. The Paperclip version and the plugins are part of the ceiling. Only the owner changes them, and they are pinned to a digest.

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
- **Paperclip provider plugin:** the one TypeScript exception to Go.
  - It lives in `integrations/paperclip-provider/`, with its own `package.json`.
  - It stays a thin client of the tenant API: all security logic lives in wsm, and its only runtime dependency is Paperclip's plugin SDK (plan §5.7).
- **Paperclip patches** are allowed only as small, separate patches in the control-room image, mainly to block local execution (plan E11, §13.6). Re-test each patch on every Paperclip upgrade.
- **Frontend:** lives in `web/`. Vue 3 + TypeScript + Vite is a proposal; confirm with the owner before scaffolding it.
- **Naming:** `wsm` is a working name. The module path is `github.com/JCombee/wsm`. Ask the owner before renaming either.
