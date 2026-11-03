# 0004: Create project users at runtime through wsm-helper

## Status

Accepted.

## Context

Each project runs under its own UID. Tools need a user record for that UID: `newuidmap`, linger and the systemd user session depend on one. Creating these users at runtime requires root code, which is a sensitive part of the system. We considered a fixed pool of users created at install time with `systemd-sysusers`, Podman's `--userns=auto`, and Incus.

## Decision

Keep the approach from plan section 3.1: `wsm-helper` creates a project user when a project is created, and removes it when the project is deleted.

- The UID comes from the range 50000-59999. The UID is the identity; the name is derived from it and never from user input.
- The user has no password and no login shell.
- The manager's database is the source of truth.

## Consequences

- The number of projects is limited only by the UID range, and no unused users exist on the host.
- `wsm-helper` keeps its privileged user management, so its code is the main risk to keep small. It accepts only validated IDs, calls standard tools with fixed arguments, and serves only the `wsm` user (invariant 4).
- Isolation tests must show that a project user cannot log in, and that deleting a project leaves no user, files or Podman state behind.
- A compromised manager can ask the helper to create or remove users within the UID range, but gets no way to supply names, paths or commands.
- The fixed pool remains the fallback if this code proves hard to keep safe.
