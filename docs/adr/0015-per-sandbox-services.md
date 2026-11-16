# 0015: Per-sandbox services

## Status

Accepted.

## Context

ADR 0013 introduced blocks with tags and a per-project ceiling, and ran shared services (MariaDB) under `wsm-svc`. It left per-sandbox blocks for later: "A per-sandbox block (Redis, M3d) gets its own life cycle." Plan sections 3.4 and 3.6 say a sandbox is a pod and that Redis runs as a container in that pod on `localhost:6379`, without an account. Plan 3.9 says `service_enable` starts the per-sandbox container.

The M3d preflight (16 November 2026, VM on `debian-13-public`, Podman 5.4.2, through the real `install.sh`, `wsm project create` and `wsm sandbox create|start`) proved the spec below for every Redis tag from 7.2 to 8.10:
- in the container: `Uid` 1000, `CapEff` and `CapBnd` 0, `NoNewPrivs` 1 and `Seccomp` 2; on the host `redis-server` runs as the project user;
- the root filesystem is read-only and `/data` is a tmpfs that uid 1000 cannot write; with `--save ""` and `--appendonly no` Redis writes nothing;
- no volumes and no port bindings; in the pod only `127.0.0.1:6379` listens, without tcp6;
- from another sandbox of the same project and from another project, `127.0.0.1:6379`, `169.254.1.2:6379` and the host address on 6379 are unreachable; on the host `127.0.0.1:6379` is refused;
- after `wsm sandbox stop` and `start` the container runs again and answers; the data is gone. `wsm sandbox delete` removes the container with the pod and leaves no volume;
- a libpod pull of an unknown tag answers HTTP 200 with `manifest unknown` in the stream;
- creating the container takes 5.4–6.0 s, starting it 0.1 s, and `redis-cli ping` answers `PONG\n` on the first try after 0.15–0.2 s.

## Decision

1. **A per-sandbox service is a container in the sandbox's pod.** Redis (`docker.io/library/redis`, `KindPerSandbox`, port 6379, profile `dev`) runs as `sbx-<id>-redis` in pod `sbx-<id>`. It shares the pod's network namespace, so the workspace container reaches it on `127.0.0.1:6379` and nothing outside the pod does. There is no account, no password or ACL, and no nftables rule: the pod's own network namespace is the boundary. One Redis per sandbox.
2. **The spec** (`sandbox.ServiceSpec`, a pure function like `WorkSpec`):
   - user `1000:1000`, `cap_drop: ALL`, `no_new_privileges`, the default seccomp profile, `read_only_filesystem` and `image_volume_mode: tmpfs`;
   - 0.5 CPU (quota 50000 on period 100000), 256 MiB memory and 64 pids;
   - command `redis-server --bind 127.0.0.1 --port 6379 --save "" --appendonly no --maxmemory 200mb --maxmemory-policy noeviction`. The image's entrypoint adds `--protected-mode no`; the real limit is `--bind 127.0.0.1` inside the pod's namespace;
   - labels `wsm.project`, `wsm.sandbox`, `wsm.service` and `wsm.tag`;
   - no volumes, no ports, no host paths, no env and no init. `podman.ContainerSpec` gains only `read_only_filesystem` and `image_volume_mode`; it still has no field for ports or bind mounts.
3. **No persistence, and another tag replaces the container.** The data lives in memory and is gone after a stop and start of the sandbox. When the agent asks for another tag, the old container is removed (force) and the new tag starts on the same port; the data is lost. Both decided by the owner on 16 November 2026.
4. **A minimum tag per block.** `Block.MinTag` is `10.11` for MariaDB and `7.2` for Redis. `blocks.ValidTagFormat` checks only the shape (MAJOR.MINOR[.PATCH], 1–3 digits per part, no leading zero); `Block.ValidTag` checks the shape and the block's minimum. The MCP checks only the shape; the ceiling does the rest.
5. **The ceiling.** `wsm project block set` for a per-sandbox block pulls each tag as the project user (`PullServiceImage`, `policy=missing`, without the manager's lock). `manifest unknown` gives `sandbox.ErrUnknownTag`, the broker maps it to `ErrInvalid`, the API answers 400, and no ceiling is stored. A ceiling that leaves out a tag a sandbox uses, and `block remove` while a sandbox has the block, give 409 naming the sandboxes, as for grants (ADR 0014). The images stay until the project user is removed.
6. **`service_enable` for a per-sandbox block.** The broker checks the sandbox and the ceiling (an empty tag is the ceiling's first tag), then calls `Manager.EnsureService` with a deadline of 60 seconds for the whole call. `EnsureService`:
   1. takes `Manager.mu` with `TryLock` in a loop, giving up at the deadline (image builds can hold it for up to 30 minutes);
   2. denies a sandbox without MCP token hash (being deleted), and answers "not available" for one that is not running;
   3. calls `record`, which takes `grantMu`, checks the token hash and the ceiling again and stores the tag in `sandbox_services`. An error from `record` is returned as is, and the container is left alone;
   4. replaces a container with another `wsm.tag`, pulls and creates a missing one, and starts it (a 304 is not an error);
   5. runs `redis-cli -h 127.0.0.1 -p 6379 ping` every 200 ms until the bounded output is exactly `PONG\n`.

   Only then does the tool answer `{block, tag, host: "127.0.0.1", port: 6379}`, without user, password or database (those fields are `omitempty`; MariaDB always sets them). "Not available" becomes `ErrUnavailable` ("service not available, try again later"), a denial `ErrDenied`. If `record` succeeded but the container did not come up, the row keeps the new tag and the next call repairs the container. The audit detail on success is `granted redis <tag>`.
7. **Lock order: `Manager.mu`, then `grantMu`.** This is the order `DeleteSandbox` uses (it holds `Manager.mu` and `RevokeSandbox` takes `grantMu`). Nothing takes `Manager.mu` while it holds `grantMu` or `Broker.mu`. Two calls for the same sandbox run one after the other, so the row and the container agree.
8. **Table `sandbox_services`** (migration `00008`): `(sandbox_id, block_id)` as primary key with the tag. `sandbox_id` has `ON DELETE CASCADE`, unlike `service_grants`: the container lives in the pod, and `DeleteSandbox` removes the pod (force) before it deletes the row, so nothing outside the pod is left to clean up. `services_available` takes `enabled_tags` and `sandbox_info` its per-sandbox services from this table. `RevokeSandbox` and `RestoreGrants` stay about grants only.
9. **Start.** `wsm sandbox start` starts the pod, and with it the service container. Redis comes back empty.

## Consequences

- An agent gets a private Redis with one tool call. The first call takes about 6 seconds to create the container, plus the pull if the ceiling's image is gone.
- `service_enable` for Redis can wait for the manager's lock; while a build holds it, the call answers "service not available, try again later" after 60 seconds.
- Switching tags or stopping the sandbox loses the data, by design. Agents that need durable data use MariaDB.
- There are no API or CLI commands to list or stop per-sandbox services yet; they come with the dashboard in M4. Other per-sandbox blocks (Valkey, Mailpit) need a runtime in `internal/sandbox` and an entry in the catalog.
