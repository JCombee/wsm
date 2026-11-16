# 0013: Shared services per version

## Status

Accepted.

## Context

Plan section 3.6 has one shared MariaDB that the broker hands out per sandbox, and section 3.12 describes a block catalog in YAML. Section 8 left the MariaDB version open. M3b builds the shared instances and the network rules that let a project reach them. The owner decided on 15 November 2026: "we should be able to run several versions of every service; the workspace we connect can ask for a certain version or tag". M3c adds MariaDB accounts through the MCP, and M3d adds Redis per sandbox.

This ADR records the lasting choices of M3b, including three deviations from the plan. `docs/plan/` stays unchanged.

## Decision

1. **Several versions, one instance per (block, tag).**
   - A shared block can run in several versions side by side. Each tag (`11.8`, `12.3`, …) gets one instance, and every project whose ceiling allows that tag shares it.
   - Tags are `MAJOR.MINOR` or `MAJOR.MINOR.PATCH`: decimal numbers without leading zeros, at most three digits each, and at least `10.11`. There is no `latest` and no suffix. Whether a tag exists shows at the pull.
   - *Deviation from plan 3.6 and section 8,* decided by the owner.
2. **The catalog is Go code.** `internal/blocks` holds the catalog: `Block` with ID, kind (`shared` or `per-sandbox`), image repository without a tag, container port and profiles (the first one is the default). M3b has only `mariadb`: shared, `docker.io/library/mariadb`, port 3306, profile `dev`. The package uses only the standard library, so `wsm-helper` validates service and tag with the same code. *Deviation from plan 3.12* (YAML), decided by the owner.
3. **The ceiling** of a project lives in `project_blocks` (migration `00006`): block, profile and `config_json` = `{"tags": [...]}` with 1 to 8 tags without duplicates. The first tag is the default for M3c. It changes only through the API and the CLI (`wsm project block list|set|remove`), never where an agent can write (CLAUDE.md invariant 5). Removing a ceiling leaves the instances alone.
4. **Instances run under `wsm-svc` as rootless Quadlet units.**
   - `install.sh` only prepares the user: a system user with home `/var/lib/wsm-svc` (0700), shell `nologin`, subordinate IDs `1879048192-1879113727`, linger, and the directory `/etc/containers/systemd/users/<svc-uid>` (root, 0755). It pulls no images and installs no units. *Deviation from plan 7*, which installed one MariaDB unit.
   - `wsm-helper` writes one unit per instance, `wsm-<block>-<slug>.container` (for example `wsm-mariadb-11-8.container`), from a fixed template, as root with mode 0644 and without following symlinks. The only variable parts are derived from the validated block, tag and port. The unit publishes `10.255.0.1:<port>` to the container port, limits memory to 1 GiB and pids to 512, sets `MARIADB_AUTO_UPGRADE=1`, and uses `Restart=always`, `RestartSec=5` and `TimeoutStartSec=900`.
   - The helper never takes image names, paths, unit text or commands from a request. The four new request types (`pull_service_image`, `ensure_service_instance`, `allow_service`, `revoke_service`) each take exactly their own fields, and every other field is an error, also for the existing types.
5. **Podman for `wsm-svc` runs only inside `wsm-svc`'s own user manager.** The helper runs every `podman` command as `runuser -u wsm-svc -- … systemd-run --user --wait --pipe --quiet --collect -p RuntimeMaxSec=600 -- podman …`. `systemctl --user` goes directly through `runuser`, since it is only a D-Bus call.
   - Reason (preflight 2): the first `podman` call of a user creates its pause process, which keeps the mount namespace of the caller. Started from the helper, that is the helper's `PrivateTmp`. Every later Podman action of `wsm-svc`, including the container starts of its user manager, then lands in a namespace whose `/tmp` and `/var/tmp` are gone once the helper stops or restarts. pasta then fails with `Failed to mount empty tmpfs for pivot_root()` and pulls fail on `/var/tmp`. Only a restart of `user@<svc-uid>.service` fixed it.
   - `--wait` passes podman's exit code back, and `--pipe` passes stdin as a pipe, which `podman secret create -` needs.
   - We keep `PrivateTmp=yes` on `wsm-helper.service`; Podman inherits nothing from the helper.
6. **Ports.** Instances publish on `10.255.0.1` only, on ports 20000–20999 (`netpolicy.ServicePortMin/Max`, repeated in a `CHECK` constraint). The manager hands out the lowest free port in the same transaction as the insert. The port of an instance never changes.
7. **The root password.**
   - The manager makes it per instance: 32 bytes from `crypto/rand`, base64url without padding (43 characters). It stores it only sealed (`internal/secret`, ADR 0009), with the instance ID as additional data.
   - It travels over the helper socket in the request and then on stdin into a Podman secret `wsm-<block>-<slug>-root`, used as `Secret=…,type=env,target=MARIADB_ROOT_PASSWORD`. It is never in argv, a unit, a log, a job, an error message, `podman inspect` (`Config.Env`) or API and CLI output.
   - Podman's file driver keeps secrets base64-encoded in `wsm-svc`'s home, which is 0700.
   - It never changes. The MariaDB image uses it only to initialise an empty volume, so a new password would lock the manager out. A failed start keeps the row and the password, so a next attempt uses the same ones. Losing the database therefore means the volumes have to be recreated by hand.
8. **Pull, ensure and readiness.**
   - `wsm project block set` is synchronous. For a tag without an instance, the broker pulls first, so a tag that does not exist leaves no row, unit or secret. It then stores the instance, ensures every instance of the ceiling (also existing ones, which repairs them and keeps the unit equal to the template), and waits up to 5 minutes until each is ready. Only then is the ceiling stored.
   - Ready means: a TCP connection to `10.255.0.1:<port>` gets a MariaDB greeting with protocol 10 whose server version, after an optional `5.5.5-`, equals the tag or continues it with `.` or `-`. At most 1 KiB is read, within 2 s; the greeting is untrusted input. The manager does not log in (that is M3c).
   - `wsm service list` checks every instance live; there is no stored status that can go stale.
   - `wsm serve` ensures every instance in the background after loading the base rules. Errors are logged and not fatal, and it does not wait for readiness.
   - Images are pulled only the first time a tag is used. There is no auto-update: `11.8` stays on the patch of its first pull. Instances are never removed or updated automatically; removing and updating are follow-up work.
9. **Grants are elements in `svc_allow4`.**
   - A grant is `uid . 10.255.0.1 . port`, added with `add element` (idempotent) and removed with `destroy element` (no error when missing). Because the rule matches `meta skuid`, a grant holds per project UID, so for all sandboxes of that project.
   - `delete_project_user` first removes every element of the UID with one `nft -f -` script of `destroy element` lines over the whole port range. It needs no listing, which could exceed the helper's 16 KiB output cap. UIDs are reused, so this must happen before the user goes. Without table `inet wsm` there is nothing to remove.
   - `ApplyBaseRules` replaces the whole table and so empties the set at every start of `wsm serve`. M3b has no grants in the database; M3c must restore them after the base rules.
   - M3b adds the helper operations and the client methods. Only the isolation tests call them; the manager does from M3c on.
10. **Connections arrive as `10.255.0.1`.** pasta forwards every connection, from the host, the manager or a sandbox, so MariaDB sees `10.255.0.1` as the client address. Accounts in M3c are therefore `'%'`; isolation comes from nftables and an account per sandbox.

## Consequences

- One instance per (block, tag) keeps memory use predictable: projects on the same tag share one server. It also means one project's load can affect another's on the same tag; accounts in M3c get `MAX_USER_CONNECTIONS`.
- The helper serves one request at a time, so a pull holds every other helper request for up to 10 minutes. Pull and ensure get a 10-minute timeout in the helper and an 11-minute deadline in the client, so the helper's own error arrives first. Docker Hub limits anonymous pulls per IP, which is why images are pulled only when missing.
- The readiness probe and `wsm service list` open a TCP connection and close it after the greeting, which MariaDB counts as an aborted connection. Enough of those can make MariaDB block a host (`max_connect_errors`), and every client arrives as `10.255.0.1`. The official image ships a configuration with `skip-name-resolve` and host cache size 0, which turns that blocking off; M3c must confirm this on the running instances before accounts depend on it. Confirmed in M3c, see ADR 0014.
- A database restore without the matching volumes, or the reverse, leaves instances the manager cannot log in to. Fixing that is manual.
- A new block is a code change in `internal/blocks`, reviewed like any other. A per-sandbox block (Redis, M3d) gets its own life cycle.
- Follow-up work: `wsm service remove|update`, updating floating tags, pruning images, and TLS to MariaDB.
