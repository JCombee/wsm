# 0003: Thin REST client for Podman

## Status

Accepted.

## Context

Plan section 2 names the official Podman bindings as first choice, to be validated in spike 02, with a thin REST client as the alternative. Both worked in the spike. The bindings probe built statically with `CGO_ENABLED=0`, but came to 36.4 MB with 120 listed dependencies, and it only exercised a basic connection. The REST client used only the standard library plus `github.com/coder/websocket` and covered pod creation, hardening, exec with TTY and resize against Podman 5.7.0. See `spikes/02-podman-api/FINDINGS.md`.

## Decision

`internal/podman` uses a thin client of our own over the libpod REST API on each project user's socket. We do not depend on the Podman bindings.

## Consequences

- The dependency tree and binary stay small, in line with the minimal-dependency rule.
- We maintain the request and response types for the endpoints we use, and only those.
- API changes between Podman versions are ours to catch. The target's minimum version needs testing, because the spike so far ran against 5.7.0 only.
- Responses from the socket come from a process the project user controls, so they are untrusted input: enforce size limits and parse strictly (invariant 8).
