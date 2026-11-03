# Findings: spike 01 network

- **Run:** 2 November 2026, on host `alien`: Ubuntu 26.04.1, kernel 7.0.0-38, Podman 5.7.0, nftables 1.1.6, pasta 2026-01-20, LAN address 192.168.0.133, no global IPv6.
- **Environment caveat:** this was not the intended Debian 13 VM. The network details (`enp2s0`, `wlp3s0`, ufw rules) suggest a physical machine. The owner needs to confirm whether it is disposable.
- **Outcome:** no-go for M1 for now. One criterion failed on a test artefact, and the run could not test a host with a public address.

## Criteria

| ID | Result | Evidence |
|---|---|---|
| `NET-DNS` | PASS | `getent hosts deb.debian.org` resolves in `sbx-a-work` |
| `NET-INTERNET` | PASS | HTTP response from `deb.debian.org:80` |
| `NET-MARIA-BEFORE` | PASS | root connects; sandbox refused, `wsm0_blocked` 0 -> 1 |
| `NET-MARIA-AFTER` | PASS | `CURRENT_USER()` is `sbx_a@%`, `svc_allowed` 0 -> 1 |
| `NET-MARIA-OTHER` | PASS | project B stays blocked after A's rule |
| `NET-LAN` | FAIL (test artefact) | 6 of 7 probes moved `lan_blocked`; see below |
| `NET-METADATA` | PASS | `linklocal_blocked` 0 -> 3, from sandbox and UID. No real metadata service on this network |
| `NET-LOOPBACK` | PASS | `loopback_blocked` 0 -> 4 for IPv4 and IPv6 |
| `NET-CROSS-PREVIEW` | PASS | root connects to `127.0.0.1:18080`; project A refused on every path tried |
| `NET-WSM0-OTHER-PORT` | PASS | root connects to `10.255.0.1:9999`; sandbox refused |

## What we learned

- **`NET-LAN` failed on the host's own address, and this is not a leak.** The probe to `192.168.0.133` was refused without moving the counter. pasta copies the host address into the namespace: `/etc/hosts` in the container maps `192.168.0.133` to the pod itself. The packet therefore never leaves the namespace. The test wrongly required a counter increase for this address. This explanation comes from the logs; it has not been confirmed by a rerun.
- **`meta skuid` sees pasta's outbound traffic.** The UID-based counters moved for probes made from inside the container.
- **Podman maps `169.254.1.2` and `host.containers.internal` to the host.** It starts pasta with `--map-guest-addr 169.254.1.2`. These probes were rejected, and the final `lan_blocked` count of 13 fits them being rejected by the private-range rule (7 from `NET-LAN`, 6 from the later gateway and host-mapping probes). This is inferred from counter totals, because those probes did not log per-probe counters.

## Surprises and open questions

- **Gap, not tested: hosts with a public address.** The block depends on the host address being private. On a public address, `169.254.1.2` most likely reaches host services bound to `0.0.0.0`, because no rule in plan section 3.3 covers it. Candidate fix: reject `fib daddr type local` for the project UID range, after the allow rules. The spike's public-address probe did not run here.
- **IPv6 is barely tested.** Only `::1` was probed. `fc00::/7`, `fe80::/10` and `dns6` were not exercised, and the host has no global IPv6.
- **The container's second nameserver is `192.168.0.1`.** The private-range rule should block it. No test targeted DNS to that address specifically.
- **`ct state established,related accept` is untested in isolation.** No test removes it to show that previews need it.

## Decisions

- Filtering per UID with `meta skuid` works for pasta: see `docs/adr/0001-per-uid-egress-filter.md`.
- Blocking host-local addresses changes plan section 3.3, so the owner decides.

## Needed before M1

The tests below describe the changes made after this run; they have not run yet.

1. `NET-LAN` now requires the host's own address to fail, without a counter increase.
2. New test `NET-HOST-SERVICE`: a root listener on `0.0.0.0` must be unreachable for the project UID and from a sandbox, including through `169.254.1.2`.
3. Rerun on a disposable Debian 13 cloud VM with a public address, using `wsm-test-server`. Without a new rule, `NET-HOST-SERVICE` is expected to fail there, which would confirm the gap.
4. The owner decides on the local-address rule, then a final rerun must pass.
