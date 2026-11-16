# 0014: Service grants

## Status

Accepted.

## Context

ADR 0013 runs one shared instance per (block, tag) under `wsm-svc` and lets a project's ceiling name the tags it may use. M3c gives a sandbox its own MariaDB account on such an instance through the MCP tool `service_enable` (plan sections 3.6 and 3.9). CLAUDE.md invariant 7 requires that credentials go out only after the nftables rule is active, and that deleting a sandbox revokes its accounts and drops its prefixed databases. The M3c preflight (15 November 2026, on 11.8.9 and 12.3.3, through the real helper) proved the SQL below and the order of revocation.

## Decision

1. **One grant per (sandbox, instance).** Table `service_grants` (migration `00007`) holds one row per sandbox and instance, unique on that pair. A sandbox may have grants on several tags of a block.
   - The account is `'sbx_<sandbox-id>'@'%'`, the database `sbx_<sandbox-id>_app`. Both derive from the sandbox ID and are not stored. The ID passes `db.ValidID` before it reaches SQL. All connections arrive from `10.255.0.1`, so the host part is `'%'`.
   - Each grant has its own password: 32 random bytes as base64url (43 characters), sealed with AES-256-GCM and bound to the grant ID. A second `service_enable` returns the same credentials.
   - `sandbox_id` has no `ON DELETE CASCADE`: a sandbox row can only go once its grants are revoked.
2. **Idempotent account SQL.** Every statement goes on its own over a root session of `go-sql-driver/mysql`, without TLS, without multi-statements and without client-side interpolation:
   ```sql
   CREATE USER IF NOT EXISTS 'sbx_<id>'@'%' IDENTIFIED BY '<pw>' WITH MAX_USER_CONNECTIONS 50;
   ALTER USER 'sbx_<id>'@'%' IDENTIFIED BY '<pw>' WITH MAX_USER_CONNECTIONS 50;
   GRANT ALL PRIVILEGES ON `sbx\_<id>\_%`.* TO 'sbx_<id>'@'%';
   CREATE DATABASE IF NOT EXISTS `sbx_<id>_app`;
   ```
   - `CREATE USER … IDENTIFIED BY ?` as a prepared statement fails with 1064, so the password is a literal. It is checked against `^[A-Za-z0-9_-]{43}$` first, and is replaced by `[redacted]` in any error.
   - Not `CREATE OR REPLACE USER`: that takes the privileges away for a moment.
   - Run twice, the statements keep the data (preflight). Each `service_enable` runs them, so a call also repairs the account.
   - The account sees only its own databases, may create more `sbx_<id>_*` ones, and gets 1044 elsewhere; `mysql.user` gives 1142, `CREATE USER` and `SET GLOBAL` 1227, `LOAD_FILE` NULL, and the 51st connection 1226 (M3b preflight).
3. **Revocation order:** `DROP USER IF EXISTS`, so no new login gets in; then `KILL CONNECTION` for every session in `information_schema.PROCESSLIST` with that user (1094, unknown thread, is ignored), since a dropped user's open session stays alive; then `DROP DATABASE IF EXISTS` for every name in `information_schema.SCHEMATA` that matches `sbx\_<id>\_%` and starts with `sbx_<id>_`. The underscores in the pattern are escaped, or `sbx_<id>xtest` would match. Those names come from the agent, so they are quoted with backticks and every backtick inside is doubled.
4. **nftables per (project UID, port).** The element in `svc_allow4` covers the project's UID, so all sandboxes of a project share one element per instance; within a project the per-sandbox account separates them. `Enable` adds the element after the SQL and before it returns the credentials; a failure of either returns an error and no credentials, and the row stays, so the next call is idempotent. `RevokeSandbox` removes the element only when the grant is the project's last one on that instance (counted in the database), and deletes the row after that.
5. **Restore at start.** `ApplyBaseRules` empties `svc_allow4` at every start of `wsm serve`. Right after it, before the API and the MCP open, `RestoreGrants` adds the element for every distinct (project UID, port) with a grant. Errors are logged and not fatal: that access then stays closed.
6. **A smaller ceiling is refused with 409.** `SetBlock` with tags that leave out a tag a sandbox of the project has a grant on, and `RemoveBlock` while there are grants on the block, fail with `broker.ErrConflict`, naming the sandboxes; the API answers 409 and the ceiling stays unchanged. The owner deletes those sandboxes first. The check runs once before the pulls and again with the store under `grantMu`. Decided by the owner on 15 November 2026.
7. **The race with a delete.** `DeleteSandbox` first clears the sandbox's MCP token hash (new MCP calls get 401), then removes the pod, then revokes the grants, then deletes the row. `Enable` checks the token hash at the start and again under `grantMu`, which `RevokeSandbox` holds too, so no grant can appear after the revocation. A failed revocation fails the delete (API 500, CLI exit ≠ 0) and keeps the row; a next delete continues. `DeleteProject` revokes every sandbox before it removes the project user.
8. **Locks.** `grantMu` is separate from `mu`, which SetBlock, RemoveBlock and EnsureAll hold during pulls of minutes, so `service_enable` does not wait for them. Whoever needs both takes `mu` first. `Enable` waits at most 60 seconds for a root login; after that the tool answers "service not available, try again later" and the audit event is `error`.
9. **Dependency: `github.com/go-sql-driver/mysql` v1.10.1**, with `filippo.io/edwards25519` v1.2.0 as its only indirect dependency. It is the standard MySQL/MariaDB driver for Go, pure Go, and builds static; the preflight proved it against 11.8 and 12.3. The connection uses `mysql.NewConfig()` with user root, TCP to `10.255.0.1:<port>`, and timeouts of 5 s (dial) and 30 s (read, write), so the driver's defaults (such as native password authentication) stay on. Decided by the owner on 15 November 2026.
10. **MariaDB does not block `10.255.0.1`.** On 11.8.9 and 12.3.3, `skip_name_resolve=ON`, `host_cache_size=0` and `max_connect_errors=100`: 300 connections that closed after the greeting raised `Aborted_connects` from 0 to 300, and logins still worked. This closes the open point of ADR 0013.
11. **A tag that does not exist gives 400.** When `podman pull` reports `manifest unknown`, `wsm-helper` answers with code `not_found`; the client maps it to `helper.ErrNotFound` and the broker to `ErrInvalid`, so the API answers 400 and no instance row is left. Other pull errors stay 500.
12. **The MCP tools.** `service_enable` takes `{"block", "tag"?}`, decoded strictly (exact keys, strings, block `^[a-z][a-z0-9-]{0,31}$`, tag empty or valid per `internal/blocks`), and takes the sandbox only from the token. Its outcome goes into the request's audit event: `ok` with detail `granted <block> <tag>`, `denied` with the reason, or `error`. The password is only in the tool result: never in the audit event, a log line, `sandbox_info`, `services_available`, or API or CLI output. `services_available` lists the ceiling with `block`, `profile`, `tags`, `default_tag` and the sandbox's `enabled_tags`, without live state.

## Consequences

- An agent gets a working, isolated database with one tool call, and gets the same credentials again after a restart of its tools.
- Deleting a sandbox or project can now fail on an unreachable instance; the owner retries after the instance is back. The sandbox's MCP access is gone from the first attempt on.
- The ceiling cannot shrink under running work; the owner has to delete the sandboxes that use a tag first.
- There are no API or CLI commands to list or revoke grants yet; they come with the dashboard in M4. Redis (per sandbox) follows in M3d.
- A second dependency tree (the driver) is now part of the manager binary; `wsm-helper` does not link it.
