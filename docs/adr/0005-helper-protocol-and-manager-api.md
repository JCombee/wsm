# 0005: wsm-helper protocol and manager API

## Status

Accepted.

## Context

M1 needs two local interfaces: the manager asks `wsm-helper` (root) for privileged work, and the `wsm` CLI asks the manager to manage projects and sandboxes. Invariant 4 limits the helper to validated IDs and typed requests. The CLI must work without the dashboard, which only arrives in M4.

## Decision

- **Helper protocol.** One JSON request per connection on `/run/wsm/helper.sock`, answered by one JSON response; the client half-closes after the request.
  - A request has only a `type` and, where the type needs it, a `uid`. Types in M1: `create_project_user`, `delete_project_user`, `setup_wsm0`, `apply_base_rules`.
  - The server rejects unknown types, unknown fields, a missing or extra `uid`, a UID outside 50000-59999, and messages over 4096 bytes.
  - The user name (`wsm-p-<uid>`), home, Podman socket path and subordinate ID range (`uid*65536`, 65536 IDs) are derived from the UID on the server side.
  - The peer is checked with SO_PEERCRED against the `wsm` user before the request is read.
- **Manager API.** HTTP/1.1 with JSON over the systemd-activated Unix socket `/run/wsm/api.sock`, mode 0660, group `wsm-admin`. Routes live under `/v1/` (projects, sandboxes, start/stop, delete) and `GET .../terminal` upgrades to a WebSocket that carries a TTY exec with resize messages.
- The CLI is a thin client of this API; it has no direct access to the database, Podman or the helper.

## Consequences

- The helper's attack surface is a small, closed set of request types; adding a privileged operation means adding a type, with tests in `TestRejectsInvalidRequests`.
- File permissions on the API socket are the CLI's only authentication. Anyone in `wsm-admin` is a full administrator; the dashboard (M4) will add its own authentication on top of the same handlers.
- Subordinate ID ranges never overlap those `useradd` hands out (they start at 100000 and grow by 65536), because 50000*65536 is far above them.
