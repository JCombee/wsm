# Spike 06 (S3): the tenant API — findings

**Outcome: go for P1.** Every criterion passed on the final run
`20261124T173546Z-2eb99e7-tov-7d8ee788`: a fresh `debian-13-public` VM, HEAD
`2eb99e7`, 28 of 28 PASS, `fails=0`. `SO_PEERCRED` works through rootless
Podman: a keep-id container connects as the control room's host UID, while a
container without keep-id arrives as a subuid and is refused.

The setup with the server user `s06tenant` in the groups `wsm` and `wsm-admin`
is a stand-in for the manager, built for this disposable VM only. It is not
the design. In P1 the tenant API runs inside the manager.

## S3 done-when (plan §13.2)

| Point | Result | Evidence |
|---|---|---|
| Control room A can create, use and delete only A's sandboxes | Yes | `TENANT-OWN`, `TENANT-CROSS`, `ID-OTHER`, `SANDBOX-NO-TENANT` |
| Other images, too many sandboxes and limits above the ceiling are refused | Yes | `CEIL-IMAGE`, `CEIL-LIMITS`, `CEIL-COUNT`, `SPEC-STRICT` |
| Every call is in the audit, refusals included, without environment values | Yes | `AUDIT-ALL`, `AUDIT-NOSECRETS`, `AUDIT-CHAIN`, `AUDIT-FAILCLOSED` |
| Long and parallel exec streams work, with the exit code | Yes | `EXEC-*`, `DUPLEX-*`, `XFER` |

## Criteria

From `SUMMARY.txt` of the final run.

| ID | Result | Evidence |
|---|---|---|
| `SETUP-WSM` | PASS | wsm active, both project images built (5 s each), `GET /v1/projects` as `s06tenant`: 200 |
| `SETUP-TENANT` | PASS | unit runs as `s06tenant`; dirs `s06tenant:s06-ca:750` and `s06tenant:s06-cb:750`; sockets 666; no TCP listener |
| `ID-KEEPID` | PASS | keep-id container of `s06-ca`: whoami 200, project `s06a`, peer UID 61001 |
| `ID-SUBUID` | PASS | `--user 1000:1000` without keep-id: 403 on all five calls (whoami, list, create, delete, exec); audit deny lines with peer UID 3997762535, inside the subuid range of `s06-ca` (starts at 3997761536) |
| `ID-OTHER` | PASS | `podman run` of `s06-cb` with A's socket: `statfs …: permission denied`; host connect as `s06-cb`: EACCES; B's own socket: 200, `s06b`, UID 61002 |
| `TENANT-OWN` | PASS | create 201, listed, `running`, stop 204 → `stopped`, start 204 → `running`, exec `true` 0, delete 204, gone from the management API |
| `TENANT-CROSS` | PASS | GET, start, stop, DELETE and exec with A's ID through B's socket: 404 with the same body as a random ID; A's sandbox still `running` and not in B's list |
| `CEIL-IMAGE` | PASS | another image: 403, count 0 → 0 |
| `CEIL-LIMITS` | PASS | cpu, memory and pids one above the project limits (2000/2147483648/1024): 403 each |
| `CEIL-COUNT` | PASS | max 2: 201, 201, third 403; after a delete 201 |
| `SPEC-STRICT` | PASS | unknown field, bad env name, NUL in argv, `argv[0]` with `-`, 65 args, timeout above 4 h: 400 each, before any upgrade |
| `AUDIT-ALL` | PASS | 154 client request IDs, each with one line (two for a started exec); the cancelled exec ends with reason `cancel`; 219 audit lines |
| `AUDIT-NOSECRETS` | PASS | the sentinels (env value and `argv[1:]`) appear 0 times in the audit, the journal and `results/` |
| `AUDIT-CHAIN` | PASS | `audit-verify`: ok, 219 lines; one changed byte in line 109 gives `line 110: prev does not match line 109` |
| `AUDIT-FAILCLOSED` | PASS | with `chattr +i` on the audit: create 503, sandboxes 1 → 1; writable again: 201 |
| `EXEC-EXIT` | PASS | stdout `out\n`, stderr `err\n`, code 7 |
| `EXEC-STDIN` | PASS | 8 MiB random stdin: sha256 equal |
| `EXEC-ENV` | PASS | multi-line value with `=` and quotes, and an empty value: byte-exact; host argv scan as root: env value in 0 processes |
| `EXEC-TIMEOUT` | PASS | `sleep 30` with timeout 3: exit after 3095 ms, `timed_out`, code 124, no `sleep 30` left |
| `EXEC-CANCEL` | PASS | connection dropped after 2 s: `sleep 300` gone after 164 ms; audit end reason `cancel` |
| `EXEC-PARALLEL` | PASS | 8 × `sleep 3`: all 0, in 3648 ms |
| `EXEC-LONG` | PASS | 120 lines over 120461 ms, code 0 |
| `EXEC-IDLE` | PASS | `sleep 150; echo done`: `done` after 150154 ms, code 0; no idle timeout anywhere in the chain |
| `EXEC-LIMIT` | PASS | 5 MiB stdout with a 1 MiB limit: error frame after 88 ms, process gone after 129 ms |
| `DUPLEX-ECHO` | PASS | `cat`, 1000 round trips of 1 KiB: all exact, code 0 |
| `DUPLEX-STOP` | PASS | a process that ignores TERM: exit frame after 5196 ms with `stopped`, none left (TERM, then KILL after 5 s) |
| `XFER` | PASS | 128 MiB as tar in and out: sha256 equal both ways |
| `SANDBOX-NO-TENANT` | PASS | in A's sandbox: no `/run/s06`, no socket of a tenant |

## Versions

- Debian 13.7 (trixie), kernel 6.12.115+deb13.1-cloud-amd64, 2 vCPU, 3.9 GB RAM.
- Podman 5.4.2, libpod REST API v5.0.0.
- wsm built at `a32453d`; `internal/`, `cmd/` and `deploy/` are unchanged since `76cee25`.
- `tenantspike`: Go 1.27.3, standard library only, `CGO_ENABLED=0`.

## Measurements

| What | Value |
|---|---|
| `exec-latency` | 50 × `true` in a row: median 145 ms, p95 162 ms, min 122 ms, max 173 ms. S1 went through the podman CLI: 167–236 ms |
| `create-time` | first create in a project 6154 ms (the first pod with its infra container), later ones 397–439 ms. Create, start and a first `true` are included |
| `xfer-throughput` | in 71 MiB/s, out 66 MiB/s (tar through frames and the Podman exec stream) |
| `duplex-rtt` | 1 KiB round trip: median 334 µs |
| `peer-cgroup` | `0::/user.slice/user-61001.slice/user@61001.service/user.slice/libpod-<64 hex>.scope/container` |
| `id-root` | container root without keep-id and without `--user`: peer UID 61001, accepted (rootless root is the user itself) |
| `env-on-disk` | env sentinel after the execs, in the storage and runtime dirs of the project user: none |
| `argv-host` | the unprivileged host user `test` read the argv sentinel in 3 processes (`sh`, `timeout`, `sh`); the env value in 0. Scans: root 840, `test` 841 |
| `restart-mount` | after a restart of `s06-tenant`, a running container with the mounted socket file gets `connection refused` on all 12 tries; a new container gets 200 |

With S1's 40 to 130 execs per agent run, exec overhead is about 6 to 19 s per
run.

## Answers to the open questions

**Does `SO_PEERCRED` work through rootless Podman, with the right host UID?
(plan §14)** Yes. A bind-mounted Unix socket is the host's socket itself,
with no proxy in between. The kernel therefore reports the peer's credentials
as host IDs:

- **keep-id:** the host UID of the control room user (61001). Accepted.
- **`--user 1000:1000` without keep-id:** a UID in the user's subuid range
  (3997762535). Refused.
- **container root:** the user's own UID (61001). Accepted.

The rule "peer UID is exactly the control room user's UID" therefore admits
every process of that Linux user, inside or outside the container. The Linux
user is the boundary, so P2 must keep that user for the control room alone.
The peer's cgroup names the container (`libpod-<id>.scope`). P1 can log it and,
as a second check, compare it with the control room's container ID.

**What S1 asked of S3** (`spikes/05-paperclip-provider/FINDINGS.md`):

| S1 asked | Answer |
|---|---|
| Create, start, inspect and delete own sandboxes only; another tenant's ID never resolves | Done: identical 404 for another tenant's and a random ID |
| Exec with stdin, separate stdout and stderr, the exit code, a deadline in the container, a guaranteed kill on cancel, env as a typed map with multi-line values | Done |
| A long-lived duplex stream whose stop and close end the process in the sandbox | Done: stop frame or dropped connection; TERM to the process group, KILL after 5 s |
| Tar streaming both ways with output limits | Done over exec: 128 MiB both ways, limits enforced |
| Audit for every call, without env values; quotas for count and limits | Done |
| Exec latency matters | 145 ms median through the REST API, against 167–236 ms through the CLI |

## What went wrong on the way

Three fixes during step 3. Each one is in its own commit.

1. **wsm's create does not start a sandbox** (`92a0929`). The first runs got
   504 on every create, because the readiness exec hit a stopped container
   (`container state improper`). The tenant API now creates, starts, waits for
   a `true`, and discards the sandbox when the start fails.
2. **dash's `kill` rejects `--`** (`257e488`). `kill -TERM -- -PGID` failed
   under Debian's `/bin/sh`, so the stop script fell back to TERM on `timeout`
   alone. A process that ignored TERM then lived until `timeout -k 10s`
   (10.1 s, `DUPLEX-STOP` failed). The script now passes the group as `-PGID`
   directly after the signal. It also polls every 0.1 s, so it reports `term`
   or `kill` truthfully. A unit test runs the real scripts under dash and
   bash; busybox was not available locally.
3. **The scanner as user `test` never stopped** (`2eb99e7`). Its stop file was
   under `/root/s06` (0700), so the exec phase waited for the 30-minute limit.
   This was a runner bug, not a tenant API bug.

Deviations from the brief:

- `tenantspike` has a fourth subcommand, `scan`. It watches
  `/proc/*/cmdline` for `EXEC-ENV` and `argv-host`, with the needles taken from
  the environment.
- `run.sh` has two more phases, `restart` and `audit`.
- The wrapper does not `exec timeout`. It runs `timeout` as a background job,
  with stdin passed through fd 3, and records its PID. That PID is also the
  process group, because `timeout` calls `setpgid`. The wrapper then waits and
  removes the PID file.

## What P1 must build

**Peer check.**
- Each listener belongs to one project.
- Accept a connection only when the peer UID from `SO_PEERCRED` equals that
  project's control-room UID.
- Refuse subuids and root. Refusals go to the audit with the observed UID and
  PID.
- Read the credentials once per connection, in `ConnContext`, as the spike
  does.

**Socket layout.**
- Path: `/run/wsm/tenant/<project>/tenant.sock`.
- The directory is owned by the manager, has the control room user's primary
  group as its group, and mode 0750. The socket is 0666; the directory keeps
  others out.
- Another user cannot even mount it (`statfs: permission denied`).
- wsm-helper creates the directory with the control-room user: P1's helper
  extension.
- **The socket inode must survive a manager restart.** The control room
  mounts the socket file, and a bind mount keeps the old inode. After a
  restart the running control room gets `connection refused` until it is
  recreated (`restart-mount`).
- Fix: one systemd socket unit per tenant (`wsm-tenant@<project>.socket`),
  passed to wsm, like `wsm.socket` and `wsm-helper.socket` today. The inode
  then outlives the service, and the mount stays as it is: only the tenant
  socket (invariant 14).

**Routes.**
- The ones in this spike: `whoami`, list, create, get, start, stop, delete and
  exec, all under `/v1/tenant/`, with no project ID anywhere.
- The server chooses sandbox IDs.
- Another tenant's sandbox gives the same 404 as one that does not exist.
- JSON is parsed strictly: unknown fields are refused, and bodies have size
  limits.
- Create answers only once a `true` runs in the sandbox (90 s at most). On
  failure it deletes the sandbox.

**Exec protocol.** Keep `wsm-exec.v1` as built:
- The request carries `Connection: Upgrade` and `Upgrade: wsm-exec.v1`. The
  spec is the body, and it is checked before the 101.
- Frames are 1 type byte, a 4-byte big-endian length and at most 1 MiB of
  payload.
  - Client: stdin `0x00`, end of stdin `0x01`, stop `0x02`.
  - Server: stdout `0x10`, stderr `0x11`, exit JSON `{code, timed_out, stopped, reason}` `0x12`, error `0x13`.
- Dropping the connection cancels.
- Limits:
  - argv: 1 to 64 elements, at most 256 KiB, no NUL, and `argv[0]` does not
    start with `-`;
  - env names: at most 256 names, 1 MiB in total;
  - timeout: 1 s to 4 h;
  - output: 64 MiB by default, 512 MiB at most per stream;
  - at most 32 concurrent execs per tenant (429 above that).
- Exec always runs as `1000:1000`, never privileged, with the env in the
  libpod JSON and never in argv.
- A server-side guard kills an exec 20 s after its own deadline.

**Move the exec wrapper into `wsm-fs`.** The spike's wrapper and stop script
depend on the image's `sh`, its `kill` builtin and coreutils' `timeout`:
- dash showed that this is fragile;
- busybox's `timeout` takes other flags;
- a distroless image has no shell at all.

`wsm-fs` is static and already mounted read-only at `/opt/wsm/bin`
(invariant 1). P1 should give it an exec mode that:
- sets the process group;
- writes the PID file;
- enforces the deadline;
- handles stop with TERM, then KILL after the grace period.

**Audit.**
- Write tenant calls into wsm's append-only, hash-chained audit table
  (invariant 6), not into a separate file.
- Fields: actor (the control room), peer UID and PID, route, sandbox, decision
  and reason, request ID.
- For an exec, write a start line and an end line. Record only `argv[0]`, the
  argument count and the env names, plus the exit code, duration, bytes and
  stop reason.
- Write the line before acting, and refuse with 503 when it cannot be written.

**Ceiling.**
- The image is fixed to the project's image.
- cpu, memory and pids must be at most the project's limits.
- At most `max_sandboxes` sandboxes per project.
- The spike counts every sandbox of the project, including the owner's.
  (aangenomen)

**What the manager lacks today:**
- `CreateSandbox` does not start the sandbox. P1 calls create and start
  in-process, or adds a start option.
- There are no limits per sandbox. The spike only checks the requested limits
  against the ceiling; P1 must also apply them to the pod.
- There is no origin field. The spike tells its own sandboxes apart by ID
  shape (`t` plus 10 characters). P1 needs an `origin` column (`cli`,
  `dashboard`, `tenant`) in the database.
- `max_sandboxes` is not in the project config. It belongs in the ceiling: in
  the database, changed only through the API (invariant 5).
- There is no exec with an env map, a deadline and stop. `internal/podman`
  already has `ExecCreate`, `ExecStart` with hijack and `demux`; P1 builds on
  those and on the `wsm-fs` exec mode.

## Open questions for the owner

1. **`/proc` on the host.** Every host user can read the argv of sandbox
   processes (`argv-host`), and with it the argv of other projects. Env values
   are not exposed. Should the host mount `/proc` with `hidepid=invisible`?
   That hides other users' processes from each project user and from control
   rooms. It is a host-wide change, so it needs an ADR and a check of what it
   breaks.
2. **Owner sandboxes and the ceiling.** Should sandboxes that the owner makes
   with the CLI or the dashboard count toward a control room's
   `max_sandboxes`? The spike counts them. (aangenomen)
