# 0001: Egress filter per project UID with nftables

## Status

Accepted.

## Context

Sandboxes run in rootless pods with pasta under one Linux user per project. pasta opens outbound connections on the host as that user. Sandboxes must not reach the LAN, cloud metadata, host loopback, other projects, or shared services they have not enabled. Spike 01 tested this; see `spikes/01-network/FINDINGS.md`.

## Decision

Filter in an own table `inet wsm` on the output hook, matching `meta skuid` on the project UID range 50000-59999:

1. Accept the host resolver and `10.255.0.1:7443` (MCP).
2. Accept `uid . address . port` elements added per project on `service_enable`.
3. Reject loopback, private ranges, link-local and metadata, including `10.255.0.1` on other ports, and every other address of the host itself with `fib daddr type local`.
4. Leave everything else open.

## Consequences

- Network access to a shared service applies per project, because all sandboxes of a project share a UID. Per-sandbox accounts separate the data.
- Named counters on block rules let tests tell "blocked by this rule" from "nothing listening".
- Podman maps `169.254.1.2` to the host, and spike 01 confirmed that without the `fib` rule a sandbox reaches host services bound to `0.0.0.0` on a public address. The rule comes after the allow rules, so DNS, MCP and enabled services keep working. A rerun on a fresh droplet with a public address confirmed both.
- IPv6 rules are present but barely tested.
