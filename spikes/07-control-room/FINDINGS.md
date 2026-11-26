# Spike 07 (S2): a contained control room — findings

**Status: done (26 November 2026).** The final run is `run.sh all` on a fresh small VM, followed by an e2e rerun on the same VM with the corrected watcher: `spikes/runs/20261126T074910Z-40c7f16-dirty-tov-80617b32` (22 of 23 criteria PASS). The run that found the environment leak is `spikes/runs/20261126T071823Z-40c7f16-dirty-tov-80617b32`. Paperclip's source (release commit `c0f13289`, v2026.1117.0) was read alongside, and the reasons below cite it.

## Outcome

**Go for P2, with two gaps that P2 must close first.** Paperclip contains most of itself through its own Cloud contract, with no patch to its code. That contract makes the control room a small Paperclip Cloud stack: its own PostgreSQL and object store, `authenticated` with `public` exposure, and its company and users from an operator (wsm) through trusted headers.

What holds, measured on a small VM:
- **Plugins are pinned by Paperclip itself** (invariant 16): an image-owned catalog with a digest; plugin management answers 403.
- **Runs land only in wsm sandboxes,** through the tenant API, with no local adapter or process run (invariant 12).
- **No stdio MCP, no host folders and no egress** beyond the allowed destination.
- **The symlink attack fails.**
- **No `env_deny` name and none of the control room's own secrets reaches a sandbox,** once the secrets' names are on `env_deny`.

The two gaps:
1. **Workspace runtime services run in the control room** (`WS-SERVICE`, invariant 12). A board user can define one on a run's execution workspace, and an agent can start a defined one; it runs in the Paperclip container with the project's checkout. Paperclip's volume-free mode, or a small patch, closes it; see "For P2".
2. **Paperclip passes its own process environment into some sandbox execs,** and with it the control room's secrets (the database URL, the master key, the object store keys, the tenant token, the auth secret). In S2 the tenant API strips them by name; P1 should strip by value too.

## Results (final run)

| Criterion | Result | Detail |
| --- | --- | --- |
| `SETUP-WSM`, `SETUP-TENANT` | PASS | wsm 72 s; tenant server as `s07tenant`, socket 666 in a 750 directory, no TCP listener |
| `CR-IMAGE` | PASS | base by digest, Paperclip 2026.1117.0, plugin read-only, catalog with digest |
| `CR-HARDEN` | PASS | all three pod containers: CapEff 0, `no-new-privileges`, read-only root, memory and pids limits, only their own volume (Paperclip: plus the tenant socket); port on 127.0.0.1 only |
| `CR-NO-HOST` | PASS | no Podman socket, management socket, podman binary or `CONTAINER_HOST` |
| `CR-IDENTITY` | PASS | whoami 200, peer uid 61101 (the control room's host user) |
| `CR-HEALTH` | PASS | healthy in `authenticated`; the host address does not answer |
| `PLUGIN-BUILD`, `PLUGIN-UNIT`, `PLUGIN-READY` | PASS | 29 tests; installed from the catalog, no other plugin |
| `LOCAL-PROCESS` | PASS | no marker and no process in the control room. The run does not end: it stays queued behind the verification retries (see "Runs") |
| `LOCAL-ADAPTER` | PASS | the runs succeeded in wsm sandboxes; no claude in the control room |
| `MCP-STDIO` | PASS | enabling stdio → 422, also on a tools admin's own template; no process |
| `PLUGIN-INSTALL` | PASS | CLI and API install → 403; nothing in the volume loads |
| `EGRESS-ALLOW`, `EGRESS-DENY` | PASS | 1.1.1.1:443 allowed; 8 destinations refused, drop counter 0 → 61 |
| `RUN-FILE`, `RUN-DUPLEX` | PASS | runs succeeded, issues done, bridge 200, the sandbox's commit in Paperclip's checkout; one duplex channel, no fallback |
| `SYMLINK-BUNDLE` | PASS | the plugin refused the symlink; the canary is nowhere |
| `ENV-DENY` | PASS | the process the exec started holds none of the 13 names; the tenant API passed none on |
| `ENV-SERVER-SECRETS` | PASS | no exec passed a secret's name on (0 of 851); every server-secret hit is a control-room process |
| `WS-SERVICE` | **FAIL** | project workspace: 422 (holds). Execution workspace: `PATCH` 200, start 200, and the service runs in the Paperclip container |
| `TENANT-AUDIT` | PASS | hash chain intact, 1742 lines, one peer (61101); exec allow = end (851) |

## What is built

- **Tenant API** (`spikes/06-tenant-api`): `env_deny` in the tenant config. The server strips those names from every exec's environment before Podman sees it and records the stripped names (never values) as `env_dropped` in the audit. Unit tests cover the config, the stripping and the handler over a hijacked connection (a mutation check shows the handler test fails without the stripping).
- **Plugin** (`plugin/`): the S1 provider, moved from the podman CLI to the tenant API on `/run/wsm/tenant.sock`: `tenant.ts` (JSON routes and the `wsm-exec.v1` frame stream), with `exec`, `sandboxes`, `duplex` and `sync` on top. The project comes from the socket; the driver config holds only a timeout. 29 tests, including a fake tenant server on a Unix socket.
- **Image** (`image/Containerfile`): node:24-trixie-slim pinned by digest, Paperclip 2026.1117.0 from npm, the plugin built and tested in a build stage.
  - The plugin goes in as a bundle of Paperclip's own distribution catalog (`/opt/wsm-plugins/distribution/wsm`, read-only). `image/catalog.mjs` writes the catalog with the bundle's digest, using the same algorithm as Paperclip's `distributionBundleDigest` (checked against Paperclip's function on a test tree).
  - One layout shim: `tsx` and its `esbuild` are copied to where Paperclip's own image keeps them (see below). Paperclip's code is unchanged.
- **The control room is a pod** (`vm/run.sh`, `start_cr`), rootless under its own user, with `--userns=keep-id` at pod level:

  | Container | Image | Limits | Mounts |
  | --- | --- | --- | --- |
  | Paperclip | the image above | 2 GiB, 512 pids | its named volume, and the project's tenant socket |
  | PostgreSQL 18.1 | the same image; Paperclip's own embedded-postgres binaries | 512 MiB | its named volume |
  | Versity S3 Gateway v1.11.0 (object store, posix backend) | `versity/versitygw`, by index digest | 256 MiB | its named volume |

  - Every container runs with no capabilities, `no-new-privileges` and a read-only root.
  - They talk over the pod's own loopback. Only Paperclip's port is published, on the host's loopback.
  - The secrets (database password, master key, object store key, tenant token, auth secret) reach podman through env files readable only by the control room's user, never argv.
  - At every pod start, a throwaway container of the control room image creates the bucket once and probes the store's conditional writes (`s3-conditional`).
- **VM scripts** (`vm/`): `run.sh` (phases, plus `pod` to recreate the pod), `contain.sh`, `e2e.sh`, `scenario.sh` (in the sandbox), `watch.sh` (argv and environ scanner) and `secrets.sh`.

## Findings

### Paperclip's managed config is the Cloud profile

- **The contract:** `PAPERCLIP_MANAGED_CONFIG` is one JSON document in the environment:
  - its fields are `v`, `mode: "cloud"` (the only mode), `catalogVersion`, `features`, `plugins.autoInstall` and `environments`;
  - it is overlaid at read time, so the database and the API cannot change it;
  - only feature keys of tier "managed" are allowed;
  - a key it sets cannot be switched off through the API (403 `settings_operator_managed`, measured).
- **Setting it makes the instance a Paperclip Cloud stack** (`isCloudManagedInstance`), and that has requirements (measured, each one stopped the start until met):
  - **A database:** an external PostgreSQL through `DATABASE_URL`. The embedded database is refused ("Cloud bootstrap requires DATABASE_URL from the environment").
  - **An object store:** `PAPERCLIP_STORAGE_PROVIDER=s3` with an explicit bucket and region. Local disk is refused. Run logs and run evidence go there too, so a stand-in that only satisfies the check would break the runs.
  - **One that enforces conditional writes:** the run-log journal publishes its head with `If-None-Match: *` and `If-Match` (`object-byte-journal.ts`, `doc/run-evidence-storage.md`).
    - Before the first run log, Paperclip probes the store: a put on a new key, the same put again and a put with an impossible ETag. Both repeats must fail with 412.
    - Garage v2.5.0 passed the start but failed that probe: every run ended in `adapter_failed`, "Object storage does not enforce conditional publication". Its source honours the conditional headers only on GET and on copy sources.
    - Versity S3 Gateway's posix backend enforces both, under a per-object `flock`, its default lock mode. The probe measured `put=200 repeat-if-none-match=412 if-match-wrong=412`. It uses about 25 MiB.
  - **A master key:** `PAPERCLIP_SECRETS_MASTER_KEY` in the environment; key files are not accepted.
  - **Config in the database:** after the first boot the config lives there, seeded once from the file that `onboard` writes. Environment variables still override it at every start.
- **No company through the API:** `POST /companies` answers 403 `cloud_managed`, measured. Paperclip Cloud provisions the company through trusted headers:
  - the request carries `x-paperclip-cloud-tenant-token` (it must equal `PAPERCLIP_CLOUD_TENANT_SERVER_TOKEN`), plus the user and the stack;
  - the first such request creates the stack's company, under a fixed ID derived from the stack ID, with that user as a member;
  - Paperclip reads these headers only in `authenticated` mode. In `local_trusted` every request is the implicit local board, which may not create a company on a cloud instance, so a cloud instance in `local_trusted` has no way to get a company.
- **So the control room runs `authenticated` with `public` exposure, like a Cloud stack**, and wsm is its operator:
  - every API call carries the tenant token and the owner's identity from a root-only header file (`curl -H @file`);
  - the stack owner is instance admin of the instance while `enableOwnerInstanceAdmin` is on (a managed key, set in the managed config);
  - sign-up is off. Without the token the port is not a board at all, unlike `local_trusted`, where anyone who reaches the port is instance admin.
- **Two pitfalls on the way:**
  - `onboard --yes` forces `local_trusted`. It ignores `PAPERCLIP_PUBLIC_URL` but keeps `PAPERCLIP_AUTH_BASE_URL_MODE=explicit`, and then writes an invalid config. The public URL therefore goes in as `PAPERCLIP_MANAGED_RUNTIME_PUBLIC_URL`, which only the server reads and which implies the explicit mode.
  - In `local_trusted`, Paperclip must listen on loopback. Debian 13's pasta (2025_05) forwards a published port only to the pod's own address, never its loopback, so the port could not be reached from the host at all. A test server on 127.0.0.1 got a reset, the same server on 0.0.0.0 answered, in a pod and in a plain container alike.

### The plugin, pinned by Paperclip itself (invariant 16)

- **The catalog:** Paperclip reads an image-owned catalog, `$PAPERCLIP_BUNDLED_PLUGIN_ROOT/distribution/catalog.json`.
  - It checks each bundle against its digest at every boot and refuses to start on a mismatch.
  - A key in `plugins.autoInstall` makes the boot install that bundle.
  - A managed instance loads only prebuilt, auto-installed bundles: no npm, no local folders, and an activation guard.
  - Measured: "bundled plugin auto-installed and loaded" at the first boot; status `ready`, driver `wsm`, package path in the image; no other plugin.
- **The first design would not have worked:**
  - The sandbox entry in `environments` must name a provider in `plugins.autoInstall`; otherwise Paperclip refuses to start.
  - A managed instance does not activate a plugin installed through the API.
- **A managed instance starts each plugin worker with `--import` of the tsx loader at a fixed place.** That place is `<server package>/../cli/node_modules/tsx/dist/loader.mjs`, the layout of Paperclip's own monorepo image (`/app/server`, `/app/cli`).
  - The npm package keeps tsx under the server package, so the worker failed: "Managed plugin runtime loader is missing from the current image".
  - The image copies tsx 4.23.15 and esbuild 0.28.2 to that place. It is an image-layout shim, not a code patch, but it must be re-checked on every Paperclip upgrade.
  - Paperclip's own Cloud image is a monorepo build with a Rust toolchain, too heavy for a small machine.

### The lock, measured with the operator as instance admin

| Check | Result |
| --- | --- |
| Switch `enableManagedSandboxOnly` off | 403 |
| Disable or install a plugin | 403 (hidden settings, `instance.plugins`) |
| A plugin dropped into the volume, and the CLI install | nothing loads |
| A project workspace with a host folder (`cwd`) | 422 ("This instance runs agents only in the platform-managed environment") |
| Enable a local stdio MCP connection | 422 ("cannot be enabled in authenticated public deployments without a trusted runtime host") |

- **stdio MCP:** the 422 holds on an approved template and on a tools admin's own template. No process was started.
  - The `paperclip.cognee-cloud` exception has no command: it is an HTTP client inside Paperclip, not a local process.
  - The JSON import makes only drafts.
- **Workspaces:** a `git_repo` workspace with a `file://` URL in the volume is allowed. Paperclip clones it into its own checkout, which the plugin syncs into the sandbox and back.

### Runs through the tenant API

- **RUN-FILE and RUN-DUPLEX: PASS.** Each issue ran in a wsm sandbox. The scenario script reached the bridge (`/api/agents/me` 200), committed, and the commit came back into Paperclip's checkout in the control room. 25–31 s from issue to quiet; the duplex run used one duplex channel through the tenant API, with no fallback.
- **Local adapters:** a `process` agent and a `claude_local` agent without an environment both go to wsm sandboxes (the plugin's lease lines). No marker and no process appeared in the control room (`LOCAL-PROCESS`, `LOCAL-ADAPTER`).
  - The `claude_local` agent's runs succeed.
  - The `process` agent never passes Paperclip's verification in a sandbox environment: it retries every 40 s or so with `test_environment_unavailable`, every try leases a sandbox, and its run stays queued. `contain.sh` terminates its agents when it is done.
- **The runner:** on a cloud instance, Paperclip turns a new `claude_local` agent into a Paperclip Runner agent (`paperclip_runner`, provider acpx) unless the agent asks for `runner: "legacy"`.
  - Paperclip Runner needs its own runtime in the sandbox; without it the hello probe fails (`harness_test_failed`, `acpx_remote_runtime_unverified`).
  - The legacy runner runs the claude CLI through exec, as in spike 05. The e2e agent uses it.
- **Issues:** `POST issues` with a title that an open issue already has returns 200 and that issue, not a new one. The scenarios put a timestamp in every title.
- **ENV-DENY: the tenant API dropped every `env_deny` name the runs sent,** and the runs succeeded without them.
  - `CLAUDE_CODE_MESSAGING_TOKEN` is in the scenario script's environment, but in no exec request: the tenant API never sees it.
  - `scenario.sh` reports, for every ancestor up to the process the exec started, which `env_deny` names its own initial environment holds (`env-chain`): `sh:CLAUDE_CODE_MESSAGING_TOKEN,bash:CLAUDE_CODE_MESSAGING_TOKEN,claude:-,sh:-,…`. The claude CLI does not have it; its tool processes do. The CLI sets it for them inside the sandbox.
  - `ENV-DENY` therefore judges the process the exec started and the audit, and reports names set inside the sandbox separately.
- **The sandbox environment** holds 82 names, among them the bridge key `PAPERCLIP_API_KEY` and the wsm MCP token (`WSM_MCP_TOKEN`), as in spike 05.

### Paperclip hands its own environment to the sandbox

- **What:** some sandbox execs carry Paperclip's whole process environment, and with it the control room's own secrets: `DATABASE_URL` (with the database password), `PAPERCLIP_SECRETS_MASTER_KEY`, the object store keys, the Cloud tenant token and `BETTER_AUTH_SECRET`.
  - Per claude run there are two `sh -c` execs, right before the claude CLI starts (94 names instead of the run's 82). The hello probe of each new `claude_local` agent does the same.
  - In the first full run, 14 of 470 execs passed those names on. The audit records names only, after `env_deny`, so this count is exact.
- **Why the first check missed it:**
  - The watcher scanned every 200 ms and saw one sandbox process with a server secret, but the process was gone before its owner could be read, and hits without an owner were not counted.
  - The audit check looked for values only, and the audit holds no values.
- **The checks now:**
  - `ENV-SERVER-SECRETS` also checks the secrets' names in the audit (race-free), and fails on a hit without an owner.
  - The watcher (perl) takes each process's owner with `fstat` on the same open file it reads, so a short-lived process is still attributed.
  - `audit-server-names` counts, over the whole run, the execs that passed a secret's name on.
- **The fix in S2:** the six names are on `env_deny`, so the tenant API drops them like the GitHub tokens. In the final run it dropped each of them 48 times, 0 of 851 execs passed one on, and every server-secret hit in a process environment was a control-room process (Paperclip, versitygw and Paperclip's git, uid 61101).
- **For P1 and P2:** a name list is fragile: Paperclip's environment is whatever wsm gives the control room. wsm generates these values, so the tenant API can also drop any variable whose value contains one of them. Better still, the control room's process environment could hold no secret at all; Paperclip reads several of them only from the environment, though (the master key, `DATABASE_URL`).

### Symlinks out of the sandbox

- **SYMLINK-BUNDLE: PASS.** In run 1 the agent commits only a symlink to a file in the control room's volume (`leak -> /paperclip/s07-canary.txt`).
  - The plugin's sync-out refuses to archive it: "s07 archive refused: symlink leak points to an absolute path". The run fails, and the commit never reaches Paperclip's checkout.
  - In run 2 the sandbox finds no `leak`; the canary appears in no log or result.
- The refusal is the plugin's tar check from spike 05. For P2, the same check belongs in wsm: the plugin stays a thin client.

### Workspace runtime services (the open gap)

- A project workspace service cannot start without a host folder (422).
- A run's execution workspace does have a folder: Paperclip's checkout in the control room.
  - A board user with `runtime:manage` may write the workspace's runtime config (`PATCH /execution-workspaces/:id`, `config.workspaceRuntime`) and start it. The start route refuses only remote-storage workspaces.
  - **Measured: it runs in the control room.** `PATCH` → 200 and `runtime-services/start` → 200 on each execution workspace; the service's marker file and its `sleep 300` appeared in the Paperclip container. That breaks invariant 12.
- An agent cannot write a service command: Paperclip refuses host workspace command changes from agents (`assertNoAgentHostWorkspaceCommandMutation`). It may start the services of a workspace whose issue is assigned to it or its reports (`workspace-runtime-service-authz.ts`). A board-defined service can therefore be started by an agent, and it then runs in the control room.
- No hidden setting covers runtime services. The operator can hide only `instance.*`, `company.*` and `workspaces.isolation` keys.
- **Why S2's workspaces are host checkouts** (source, `remote-workspaces.ts`, `heartbeat/remote-workspace-source.ts`):
  - A run gets a remote workspace only when all of these hold: it is a Paperclip Runner run (`paperclip_runner`) in a sandbox environment; `remoteWorkspaceStorageEnabled` is on (a managed key, default on); S3, PostgreSQL and the master key are configured; and the repository URL is remote (https, ssh or git; never `file:`).
  - The legacy runner and S2's `file://` repositories always give host authority: the checkout lives in the control room.
- **Paperclip's own answer is the volume-free server:** with `PAPERCLIP_EPHEMERAL_SERVER=true`, every run must be a Paperclip Runner run with remote workspace storage ("Volume-free servers require native Paperclip Runner execution with remote workspace storage"). The control room then holds no checkout for a service to start in.
  - This is read from the source, not measured.
  - It needs the Paperclip Runner runtime in the sandbox (the hello probe failed without it) and remote repository URLs.

### Small machines

- **Size:** Paperclip's npm package is 3.1 GB, plus 1.9 GB of npm cache during the install.
  - About 2.1 GB of it are native agent CLIs: codex and the claude-agent-sdk binary each twice, opencode, the copilot CLI and its `provider-assets` copy, and the cursor SDK binary.
  - A rootless `podman build` needs about three times a layer's size to commit it, so the first build ran out of disk on the small VM (13 GB).
  - The image deletes those binaries in the install step. They never reach a layer, and a control room cannot start a local agent CLI anyway (invariant 12). `dist/vendor/paperclip-runner` (about 200 MB) stays: it is probably Paperclip Runner's sandbox runtime.
- **Each new image version costs a second copy at its first start:**
  - With `--userns=keep-id`, rootless Podman (native overlay) makes a chowned copy of the image's layers. On the small VM that takes about 6.5 minutes and 1.5 GB for the 3.2 GiB image.
  - The old image is held by the running pod until the new pod starts. `setup` prunes it then; before that fix the disk fell to 2.9 GB free.
- **A transport bug, fixed:** frames that arrived right after the 101 could be handled before the bytes that came with it, because the data listener was attached before the head was processed. The streaming test caught it.

## Measurements (final run; small VM: 2 vCPU, 3922 MiB, no swap, 13 GB)

- **Build and install times:**
  - wsm install: 72 s;
  - sandbox image: 51 s;
  - control room image: 416 s (3247 MiB).
- **Pod start:**
  - first start of a new image: PostgreSQL and the object store after 390 s (the keep-id copy), Paperclip healthy after 423 s;
  - restart: 3 s for both, and Paperclip healthy after 31 s.
- **Memory** (cgroup, current/peak, during the runs):
  - Paperclip: 930–1020 / 1161 MiB;
  - PostgreSQL: 240–290 / 296 MiB;
  - versitygw: 14–23 / 24 MiB;
  - host: 1.8 GB used, 2.1 GB available at the end.
- **Disk:** 916 MB at start; 2.3 GB after wsm and the sandbox image; 4.9 GB after the control room image; 6.6 GB with the pod running; 7.4 GB at the end (4.7 GB free). Volumes: Paperclip 3.6 MB, PostgreSQL 99 MB, object store 1.7 MB.
- **A whole `run.sh all`** takes about 26 minutes, 16 of them for the image build and the first pod start.

## For P2

Each point needs an ADR when P2 settles it.

1. **Close the workspace services gap (must, invariant 12).** Two ways:
   - **Paperclip's volume-free mode** (`PAPERCLIP_EPHEMERAL_SERVER=true`): every run must be a Paperclip Runner run with remote workspace storage, so the control room holds no checkout to run a service in. It needs the Paperclip Runner runtime in the sandbox image and repository URLs that are not `file:`. It is Paperclip's own design, but S2 did not measure it.
   - **A small patch in the control-room image:** the runtime service routes refuse host workspaces while `enableManagedSandboxOnly` is on. CLAUDE.md allows it (plan E11); it must be re-tested on every upgrade.
2. **Keep the control room's secrets out of exec environments** (must: a sandbox that holds the tenant token or the database URL holds the control room). The tenant API strips the control room's secret names (S2) and, in P1, every value wsm generated for that control room. Better still, give Paperclip as few secrets in its environment as it allows.
3. **The control room is a pod:** Paperclip, PostgreSQL and an object store. Their secrets belong to wsm, which hands them over through env files.
   - The object store must enforce conditional writes. wsm should probe that the way Paperclip does before it starts a control room on it. Versity S3 Gateway's posix backend does, in about 25 MiB; Garage does not.
   - Alternatively, wsm runs one PostgreSQL and one object store for all control rooms, each with its own database, bucket and credentials. That is cheaper on small machines, but crosses control rooms in one service.
4. **wsm is the Paperclip Cloud operator:**
   - it holds the tenant token;
   - it provisions the company through trusted headers;
   - its dashboard proxy injects the owner's identity, which gives the owner board access without a Paperclip password.
5. **The runner:** legacy (as in S2), or Paperclip Runner with its runtime in the sandbox image. Option 1's volume-free mode needs the second.
6. **Image layout:** keep the tsx shim (re-tested on each upgrade), or build from Paperclip's monorepo like its Cloud image.
7. **Strict parsing of sandbox output belongs in wsm** (invariant 8). In S2 the plugin's tar check refuses absolute symlinks; P2 moves that check into wsm, so the plugin stays a thin client.
8. **Smaller points:**
   - Lock environment and adapter management with the hidden settings `instance.environments` and `instance.adapters` (not measured).
   - Clean up agents that fail verification in a sandbox environment; each retry takes a lease.
   - Make issue titles unique, or expect Paperclip's dedup.
