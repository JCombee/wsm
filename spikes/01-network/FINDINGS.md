# Findings: spike 01 network

- **Latest run:** 3 November 2026, `20261103T195440Z-2ee7611`, on a DigitalOcean droplet (KVM): Debian 13, kernel 6.12.113, Podman 5.4.2, nftables 1.1.3, with a public IPv4 address.
- **Earlier run:** 2 November 2026 on host `alien` (Ubuntu 26.04, Podman 5.7.0, private address). That machine looked physical, so its results count as supporting evidence only.
- **Outcome:** no-go for M1. `NET-HOST-SERVICE` confirms a real gap on hosts with a public address.

## Criteria (latest run)

| ID | Result | Evidence |
|---|---|---|
| `NET-DNS` | PASS | `getent hosts deb.debian.org` resolves in `sbx-a-work` |
| `NET-INTERNET` | PASS | HTTP response from `deb.debian.org:80` |
| `NET-MARIA-BEFORE` | PASS | root connects; sandbox refused by the `wsm0` rule |
| `NET-MARIA-AFTER` | PASS | sandbox logs in as `sbx_a` after the rule is added |
| `NET-MARIA-OTHER` | PASS | project B stays blocked after A's rule |
| `NET-LAN` | PASS | `lan_blocked` 0 -> 6 for the private test addresses |
| `NET-METADATA` | PASS | `linklocal_blocked` 0 -> 3. The run has no control proving the droplet's metadata service answers for root |
| `NET-LOOPBACK` | PASS | `loopback_blocked` 0 -> 4 for IPv4 and IPv6 |
| `NET-CROSS-PREVIEW` | PASS | root connects to `127.0.0.1:18080`; project A does not, on every path tried |
| `NET-WSM0-OTHER-PORT` | PASS | root connects to `10.255.0.1:9999`; sandbox refused |
| `NET-HOST-SERVICE` | **FAIL** | see below |

## What we learned

- **The gap is real.** A root listener on `0.0.0.0:18082` accepted connections from the project UID on the host address, and from inside `sbx-a-work` through `169.254.1.2` and `host.containers.internal`. Any host service bound to all addresses is therefore reachable from a sandbox on a host with a public address. On a private address the LAN rule happens to block this, which is why the run on `alien` could not show it.
- **Services bound to specific addresses stayed out of reach.** B's preview on `127.0.0.1` and MariaDB on `10.255.0.1` were not reachable through the host mapping.
- **The `NET-LAN` failure on `alien` was a test artefact.** The corrected test passes. The container's `/etc/hosts` again maps the host address to the pod itself, matching the Podman documentation that pasta copies host addresses.
- **`meta skuid` sees pasta's outbound traffic,** on both Podman 5.4.2 and 5.7.0.

## Surprises and open questions

- **The public gateway is not blocked.** Probes to it timed out (rc=124) instead of being rejected. Plan section 3.3 leaves non-private addresses open, so other public hosts in the same network count as internet.
- **IPv6 is barely tested.** Only `::1` was probed, and `dns6` stayed empty.
- **The droplet's second container nameserver is a private address.** The private-range rule should block it; no test targets it.
- **`ct state established,related accept` is untested in isolation.**

## Decisions

- Filtering per UID with `meta skuid` works for pasta: see `docs/adr/0001-per-uid-egress-filter.md`.
- The owner approved rejecting `fib daddr type local` for the project UID range, after the allow rules. It is added to `nftables.nft` and plan section 3.3, but has not run yet.

## Needed before M1

A rerun on a disposable server with a public address must pass `NET-HOST-SERVICE`, with DNS, MCP and MariaDB still working. The run also records whether a metadata service answers for root.
