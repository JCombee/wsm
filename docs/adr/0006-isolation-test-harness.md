# 0006: Isolation test harness

## Status

Accepted.

## Context

The isolation tests of plan section 6 must run against a real installation: real users, nftables rules, Podman and systemd. They need root to set things up and must also act as project users and as other users. Running them on a developer machine could damage it.

## Decision

- The tests live in `test/isolation/` behind the `isolation` build tag and are compiled into one static binary (`make isolation-build`).
- They run only as root on a host with `/etc/wsm-spike-disposable`. Both `make isolation-test` and `TestMain` refuse otherwise, with a non-zero exit code and before changing anything.
- They run on a disposable VM from the `debian-13-public` template, after `deploy/install.sh`. The VM needs no Go toolchain.
- Setup goes through the `wsm` CLI, like an owner would: two projects, each with a running sandbox from a test image built by the project user from `testdata/Containerfile` (`localhost/wsm-test:latest`).
- To act as another user, the test binary runs a copy of itself under `runuser` with `WSM_ISOLATION_PROBE` set. In that mode it performs one read-only probe and exits.
- Network checks read the counters of the `inet wsm` rules, so a test passes only when the expected rule dropped the traffic, not when it failed for another reason. The exception is traffic that never leaves a sandbox's network namespace and so meets no wsm rule: its own loopback, and the host's address, which pasta copies into the pod. Those checks only require the connection to fail.
- The rest of the CLI life cycle also runs end to end, because only the VM has a real Podman and tmux: stop, start again, `wsm shell` with pipes instead of a terminal, and delete.
- Results are stored in `test/isolation/runs/<run-id>/` with a `run.txt` (commit, OS, address); `LATEST` names the newest run.

## Consequences

- A milestone is done only after a VM run on its final commit.
- The tests cover only what exists; later milestones add tests for the MCP server, services, mirrors and the dashboard.
- The probe mode is part of the test binary only; it never ships in `wsm` or `wsm-helper`.
- No test covers the IPv6 rules of ADR 0001 yet. That stays an open point until M5, which brings the complete isolation tests in CI.
