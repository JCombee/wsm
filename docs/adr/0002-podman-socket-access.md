# 0002: Manager access to per-project Podman sockets

## Status

Accepted.

## Context

The manager runs as `wsm` and must control the rootless Podman of each project user. Plan section 3.1 asks for access through group `wsm`, to be confirmed by spike 02. The default socket lives under `/run/user/<uid>`, which other users cannot traverse.

## Decision

For each project user, a systemd user drop-in for `podman.socket` moves the listener to `/run/wsm-podman/<user>/podman.sock` with mode 0660. The directory is owned by the project user, group `wsm`, with mode `2750`, so the socket inherits the group.

## Consequences

- `wsm` reaches each project's socket, and another project user does not. Spike 02 verified this for `wsm-p-a` and `wsm-p-b`.
- Membership of group `wsm` grants control over all project Podman instances, so only the manager may be a member.
- The socket is never mounted into a sandbox (invariant 1).
- M1 must create the directory and drop-in when it creates a project user, and remove them when it deletes one.
