NOG NIET UITGEVOERD
===================

# Findings: spike 01 network

Not run yet. After a run on a disposable VM, replace this file using the
template in `spikes/README.md`, with evidence from `results/`.

## Criteria to fill in

| ID | What it shows | Result | Evidence |
|---|---|---|---|
| `NET-DNS` | DNS works from a sandbox | | |
| `NET-INTERNET` | the internet stays open | | |
| `NET-MARIA-BEFORE` | MariaDB on `wsm0` is blocked before `service_enable` | | |
| `NET-MARIA-AFTER` | project A can log in after its rule is set | | |
| `NET-MARIA-OTHER` | project B stays blocked | | |
| `NET-LAN` | private ranges are blocked | | |
| `NET-METADATA` | `169.254.169.254` is blocked | | |
| `NET-LOOPBACK` | host loopback is blocked | | |
| `NET-CROSS-PREVIEW` | project A cannot reach project B's preview port | | |
| `NET-WSM0-OTHER-PORT` | other ports on `wsm0` are blocked | | |

## Questions to answer

- Does `meta skuid` see all pasta traffic, including DNS forwarding?
- Is `ct state established,related accept` needed for previews, and is it safe?
- Does a public host address need a `fib daddr type local` block (see the
  `INFO` lines in `summary.txt`)?
- On a local VM `NET-METADATA` only proves the rule blocks the address; on a
  cloud VM it also proves the real metadata service is unreachable.
