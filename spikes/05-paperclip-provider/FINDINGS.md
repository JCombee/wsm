# Findings: spike 05 Paperclip provider (S1)

- **Run:** 24 November 2026, `spikes/runs/20261124T072436Z-6e2db77-tov-fb14e493`: `run.sh all` on commit `6e2db77` on a fresh VM `tov-fb14e493` (template `debian-13-public`, address 198.51.100.101), Debian 13, kernel 6.12.115. All 25 criteria PASS (`status.txt`: `fails=0`).
- **How:** the brief `briefs/2026-11-23-spike-05-paperclip-provider.md` (git-excluded). Steps 1 and 2 ran on VM `tov-16123290`. That VM was orphaned when the run hit its usage limit, and held the only VM slot until the owner deleted it. Steps 3 and 4 were then written without a VM and passed on their first VM attempt (`tov-fb14e493`, before the final run).
- **Spike-only host setup:** on the VM, Paperclip runs as user `pclip`, and `pclip` is a member of `wsm` and `wsm-admin`. That gives the plugin the management API socket and every project's Podman socket. **This is a stand-in for the spike only, never a design for production.** S2 and S3 replace it with a per-tenant socket.
- **Outcome:** go for S2 and go for S3; S1 met all four points and all 25 criteria.
  - **S2: go.** Paperclip's sandbox runs need only the provider plugin's calls: create, start and delete a sandbox, exec with stdin and an environment map, and one long-lived duplex exec. A tenant API can offer all of them; the spike's direct Podman socket is only how the plugin reached them. S2 must still decide which of Paperclip's run secrets may exist in a sandbox (see *Environment in the sandbox*).
  - **S3: go.** The operations the tenant API must offer are now known and small; see *What the tenant API (S3) must offer*.

## S1 done-when

| # | Point | Result |
|---|---|---|
| 1 | acquire, execute, sync-in, sync-out and release work | ✅ `LEASE-CYCLE`, `EXEC-*`, `SYNC-IN`, `SYNC-NATIVE` |
| 2 | the bridge works in both forms over exec, and the sandbox has no network path to Paperclip | ✅ `BRIDGE-FILE`, `BRIDGE-DUPLEX`, `NET-ISOLATION` |
| 3 | a run fetches an issue, commits locally, and the commit reaches the host workspace through restore | ✅ `RESTORE-COMMIT`, `RESTORE-DUPLEX` |
| 4 | an archive with a symlink or `../` is refused | ✅ `ARCHIVE-E2E`, and 10 tar-reader unit tests in `PLUGIN-UNIT` |

## Criteria

Evidence is in `spikes/runs/20261124T072436Z-6e2db77-tov-fb14e493/05-paperclip-provider/results/`: `summary.txt` for every ID, `e2e-<scenario>/` for the details.

| ID | Result | Evidence |
|---|---|---|
| `SETUP-PAPERCLIP` | PASS | 2026.1117.0, `local_trusted`, port 3100 only on 127.0.0.1 |
| `SETUP-WSM` | PASS | wsm active, image of project `s05` present, the unit's process has groups `wsm` and `wsm-admin` |
| `PLUGIN-BUILD` | PASS | `npm ci`, `typecheck` and `build` exit 0 (`plugin/`) |
| `PLUGIN-UNIT` | PASS | `# fail 0`; the 19 names from the brief plus 5 duplex tests (24 `S05 …` tests) |
| `PLUGIN-READY` | PASS | status `ready`, driver `wsm` |
| `EXEC-EXIT` | PASS | exit 7, stdout `out\n` and stderr `err\n` apart (`exec-check.log`) |
| `EXEC-STDIN` | PASS | 8 MiB back and forth, sha256 equal |
| `EXEC-ENV` | PASS | multi-line value byte-exact, empty stays empty, `CONTAINER_HOST` reaches the container and leaves the exec target alone |
| `EXEC-TIMEOUT` | PASS | `timedOut: true`, exit 124 after 3.2 s, no `sleep 30` left |
| `EXEC-PARALLEL` | PASS | 4 × `sleep 3` in 3.5 s |
| `EXEC-LIMIT` | PASS | error "stdout exceeds the limit of 1048576 bytes" after 112 ms |
| `LEASE-CYCLE` | PASS | 5 acquires, each with a release or destroy; no `pc-` sandbox left after any scenario |
| `SYNC-IN` | PASS | `ws-head` is the host repo's first commit (`e2e-file/runlog.txt`) |
| `BRIDGE-FILE` | PASS | `bridge-me`, `issue-get` and `issue-patch` 200, no duplex open |
| `BRIDGE-DENY` | PASS | `POST /api/companies/:id/agents` 403, forged bearer 401 |
| `NET-ISOLATION` | PASS | in `file` and `duplex`: 6 probes `000` (refused) while `bridge-me=200`; the separate probe is refused while the host control gives 200 |
| `RESTORE-COMMIT` | PASS | run `succeeded`, issue `done`, host `git log` has the sandbox's commit |
| `SECRETS-ARGV` | PASS | 0 sentinel hits in 750 scans over the four scenarios (`e2e-*/watcher.txt`) |
| `SECRETS-LOG` | PASS | sentinel in no plugin log line and nowhere in `results/`; also 0 in the journal and the run logs |
| `ARCHIVE-E2E` | PASS | the restore was refused with "s05 archive refused: symlink escape-rel points out of the root"; no escaping symlink on the host |
| `ARCHIVE-LEAK` | PASS | `leak-present=no`, `leak-read=ERR`; canary content in no run log (see *Hostile outcomes* for what this does and does not show) |
| `HOOK-ESCAPE` | PASS | no planted hook or `fsmonitor`/`hooksPath` on the host, no `/tmp` marker before or after `git status` and `git log` as `pclip` |
| `SYNC-NATIVE` | PASS | `s05 sync in`/`out` lines with files > 0 (in 2, out 4), and `RESTORE-COMMIT` passes |
| `BRIDGE-DUPLEX` | PASS | 1 duplex open, no file-bridge fallback line in the run log, bridge 200/200/200 |
| `RESTORE-DUPLEX` | PASS | run `succeeded`, issue `done`, the duplex commit on the host |

## Versions

- Paperclip 2026.1117.0 (`paperclipai` on npm), `local_trusted` on 127.0.0.1:3100, with its embedded PostgreSQL.
- Plugin: `@paperclipai/plugin-sdk` 2026.1117.0, esbuild 0.28.2, TypeScript 7.0.2 and `@types/node` 24.19.2. These are the only dependencies, all pinned.
- VM: Debian 13, kernel 6.12.115, Node v24.22.3, npm 11.19.1, podman 5.4.2, wsm at `6e2db77`.
- Image: Debian trixie-slim, Node v24.22.3, Claude Code 2.1.330, GNU tar 1.35. The mock model from spike 03 runs inside the sandbox on 127.0.0.1:8787; no real model or key was used.

## Measurements

- **Acquire** (create, start and the first successful exec): 45.8 s for the very first sandbox on a fresh VM, 0.55–0.77 s for every later one (the `s05 lease acquire … ms=` lines). The interim run's first acquire took 26.7 s.
- **Release:** 0.9 s. **Exec:** 236 ms per call, measured as 10 sequential `true` (167 ms on the earlier VM).
- **Runs** (Paperclip's `startedAt` to `finishedAt`): `file` 32.0 s (the agent's first run), `hostile-1` 32.3 s (failed after three refused restores), `hostile-2` 18.9 s, `duplex` 19.2 s. From creating the issue until nothing moves: 47, 46, 31 and 31 s.
- **Exec calls per run sandbox:** 125 in `file`, 81 in `hostile-2` (file bridge) and 42 in `duplex`. Of those, the bridge's `PAPERCLIP_SANDBOX_EXEC_CHANNEL` polls are 100, 59 and 22. The duplex bridge roughly halves the exec load of a run.
- **Memory** after the scenarios, no sandbox running: 1597 MiB used, 2324 MiB available (of 3922).
- **Disk:** 8.4 GB used, 3.7 GB free on a 13 GB disk. The image is about 1.5 GB.
- **Setup:** wsm install 65 s, Paperclip install 197 s, healthy 37 s after start, image build 62 s.

## Paperclip's behaviour

- **New agents start `paused`.** On the first wake, Paperclip runs a hello probe in the environment: stdin "Respond with hello.", and the answer must match `/\bhello\b/i`. The command must be named `claude`. After 3 failed probes the agent stays paused. The probe ran in its own lease (6 execs, two of them `claude`, and an empty sync-out in `file`).
- **`claude_local` needs `engine: "cli"`.** The default ACP engine needs `claude-agent-acp` and fails with `adapter_engine_unavailable`.
- **Follow-up runs without a PATCH:** an issue that is not set to `done` keeps waking the agent with `automation` runs, and ends `blocked`. The scenarios therefore PATCH the issue to `done` with the `X-Paperclip-Run-Id` header.
- **Restore:** according to the source, git history comes back as a bundle (`git-delta.bundle`) that Paperclip imports on the host, and the other workspace files come back as a directory transfer with the repositories' `.git` excluded. A refused archive makes Paperclip retry the transfer (three tries in `hostile-1`, `retryWorkspaceRestoreBeforeHostWrite`) and then fail the run with the provider's message. In that run nothing reached the host, not even the commit.
- **Native sync** carries tar archives as single files plus an extract command: each sync-in operation moved 1 or 2 files of 10–297 KB with one or two post-upload commands, and each sync-out 1 to 4 files.
- **Config envelope:** Paperclip adds keys to the driver config of lease hooks (`agentId`, `sandbox`, `workspaceRealization`). The plugin checks unknown keys strictly only when the operator saves the config.
- **Bridge allowlist** (`sandbox-callback-bridge.ts`): `GET /api/agents/me`, `GET` and `PATCH /api/issues/:id` and `POST /api/issues/:id/comments` are allowed; `POST /api/companies/:id/agents` is not (403). A forged bearer gets 401. `POST /runtime-tools/github/credentials` is on the allowlist and answered 401 here, because no GitHub is configured.
- **Environment in the sandbox:** the run's environment holds about 90 names. Among them are `PAPERCLIP_API_KEY` (the bridge token), `PAPERCLIP_AGENT_PRIVATE_KEY` and `PAPERCLIP_AGENT_PUBLIC_KEY` (both multi-line PEM), `PAPERCLIP_GITHUB_BROKER_TOKEN`, `PAPERCLIP_GIT_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`, `CLAUDE_CODE_MESSAGING_TOKEN`, `ANTHROPIC_API_KEY` and wsm's own `WSM_MCP_TOKEN`. Only names were recorded. The private key and the tokens reach every process in the sandbox.
- **Embedded PostgreSQL** listens on 127.0.0.1:54329 and [::1]:54329, not on all addresses.
- **Duplex switch:** `PATCH /api/instance/settings/experimental` with `{"enableSandboxDuplexBridge": true}` gives 200; the default is `false`. Paperclip uses the duplex bridge only when the switch is on, the driver declares `duplexCommandStream`, and the worker implements the open hook. When the open or the readiness check fails, it falls back to the file bridge with one stderr line in the run log. `BRIDGE-DUPLEX` therefore also fails on such a line.
- **Duplex open params carry no driver config,** only the lease ID and the command. The plugin opens a channel only on a lease it acquired or resolved before (fail closed).
- **The duplex launch command holds the bridge token and nonce** (`exec env PAPERCLIP_BRIDGE_TOKEN='…' … node <entrypoint>`), and the file bridge passes `PAPERCLIP_BRIDGE_TOKEN` as an environment value. A provider that passes the duplex command as argv exposes the token in `/proc/*/cmdline` of a host process. This plugin passes each element as an environment value and rebuilds the argument vector inside the sandbox. The watcher found no process with `PAPERCLIP_BRIDGE_TOKEN=` in argv in any scenario. Inside the sandbox the token is briefly in argv of `sh -c` and `env` before `node` starts, which Paperclip accepts ("a compromised channel can read the launch environment").
- **The host's route IDs repeat across Paperclip instances** (the same route tag on two VMs). The plugin keys channels per worker process and refuses to reopen a route ID it has seen.

## Exec semantics

- **Environment:** `-e NAME` without a value takes the value from the podman client's own environment. Multi-line values with `=` and quotes arrive byte-exact, an empty value stays empty, and the value never appears in argv. `--env-file` truncates multi-line values and is not used. Names that podman itself reads (`CONTAINER_*`, `PODMAN_*`, `XDG_*`, `GO*`, …) go as `NAME=VALUE`, so they cannot steer the client: `CONTAINER_HOST=unix:///tmp/nope.sock` reaches the container and leaves the exec target alone.
- **Timeouts:** killing a `podman --remote exec` client leaves its process running in the container. Every command therefore runs under `timeout -k 10s <N>s` inside the container; `sleep 30` with a 3 s timeout ends as `timedOut` after 3.2 s with nothing left in `/proc`. A client guard kills the client 20 s after the in-container deadline.
- **Leftover processes:** the duplex wrapper records its PID in the sandbox. Stop and close signal that PID through a second exec (TERM, KILL after 5 s) before they kill the client. In the run, Paperclip sent GOAWAY, the plugin's stop sent TERM, the gateway exited with 0 and the client ended by itself (`in-container=term client-killed=false`, 1.1 s).
- **Limits:** stdout and stderr are capped (64 MiB by default, 1 MiB in the test); going over gives an error at once, without a hang. Duplex stdout is capped at 512 MiB per channel; its stderr goes to the plugin log only, at most 4 KiB, sanitized, with token-like runs redacted.
- **Parallel:** 4 parallel 3 s execs finish together in 3.5 s.

## Hostile outcomes

- **`hostile-1`** planted, in the sandbox: a committed symlink `leak` to the host canary, uncommitted symlinks `escape-abs` (to `/etc/hostname`), `escape-rel` (`../…/etc/hostname`) and `sub/root` (to `/`), hooks in `.git/hooks`, and `core.fsmonitor` and `core.hooksPath` in `.git/config`. The plugin's tar reader refused the outbound workspace archive at `escape-rel`. Paperclip retried three times, then failed the run with that message. The host repo kept only its first commit; no hook, config or marker appeared.
- **`hostile-2`** found no `leak` in its workspace (`leak-present=no`), so it could not read the canary.
- **What this does not show:** the refusal stopped the whole restore, so the committed `leak` symlink and the planted hooks never met Paperclip's own restore path. From the source, git history comes back as a bundle and `.git` is excluded from the file transfer, so hooks and config should not travel even without the refusal. But a committed symlink to a host path *would* arrive through the bundle, and the next run's sync-in tars the host workspace (`followSymlinks` is the mapping's choice). That combination is untested; see the open questions.
- **Archive refusals are all-or-nothing.** One bad member costs the agent's whole run, including legitimate commits. That is the fail-closed choice for this spike.

## What the tenant API (S3) must offer

- Create, start, inspect and delete a sandbox of the calling tenant only, with the image fixed by the tenant's project; a project or sandbox ID of another tenant must never resolve.
- Exec with stdin, separate stdout and stderr, the exit code, an in-container deadline and a guaranteed kill of the process on cancel. The environment travels as a typed map, never as argv, and multi-line values must survive.
- A long-lived bidirectional exec stream for the duplex bridge, with a stop and a close that end the process inside the sandbox, not only the client.
- Streaming transfer in both directions for tar archives and single files, with output limits; the outbound archive is parsed strictly on the host side.
- Audit for every call, refusals included, without environment values; quotas for the number of sandboxes and for resource limits.
- About 40 to 130 execs per agent run, many of them short polls with the file bridge, so exec latency matters (now 170–240 ms per call through the Podman socket).

## Open questions for the owner

1. **Committed symlinks through the git bundle.** Should a follow-up scenario commit a host-pointing symlink without any other hostile content, to test Paperclip's bundle import and the next sync-in? Or should the provider refuse symlinks that leave the repo in its own sync-in as well?
2. **All-or-nothing refusal.** Is losing the whole run on one bad archive member acceptable, or should the provider drop bad members, report them and restore the rest?
3. **Secrets in the sandbox environment.** `PAPERCLIP_AGENT_PRIVATE_KEY`, the GitHub tokens and `CLAUDE_CODE_MESSAGING_TOKEN` reach every process in the sandbox. Which of them may stay there under wsm's model, and which should S2 strip or broker?
4. **Duplex by default.** The duplex bridge halves the exec load and worked first time, but Paperclip marks it experimental and falls back silently. Turn it on for wsm tenants, and alert on the fallback line?
5. **First-acquire latency.** The first sandbox on a fresh host took 46 s, later ones under 1 s. Should wsm warm a project (one throwaway sandbox) when it is created?
