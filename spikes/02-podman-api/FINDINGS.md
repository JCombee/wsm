# Findings: spike 02 Podman API

- **Run:** 2 November 2026, on host `alien`: Ubuntu 26.04.1, kernel 7.0.0-38, Podman 5.7.0, Go 1.26.8 for the bindings probe.
- **Environment caveat:** this was not the intended Debian 13 VM; see `spikes/01-network/FINDINGS.md`.
- **Confirmed:** on 3 November 2026, all criteria passed again on a DigitalOcean droplet (KVM) with Debian 13, kernel 6.12.113 and Podman 5.4.2 (run `20261103T195440Z-2ee7611`).
- **Outcome:** go for M1 on this spike's own criteria. All criteria passed on both Podman versions.

## Criteria

| ID | Result | Evidence |
|---|---|---|
| `POD-SOCKET-ACCESS` | PASS | `wsm` gets `_ping` OK through group `wsm`; `wsm-p-b` is denied |
| `GO-TEST` | PASS | `go vet` and `go test` clean |
| `POD-USERNS` | PASS | uid 1000 in the container is host uid 50000 |
| `POD-HARDENING` | PASS | `NoNewPrivs: 1`, `Seccomp: 2`, `CapEff: 0`, uid 1000, cpu/memory/pids limits, no mounts |
| `POD-EXEC-WS` | PASS | the marker output came back over the WebSocket |
| `POD-RESIZE` | PASS | `stty size` shows `37 123` after a resize message |
| `POD-BINDINGS-CGO0` | PASS | static binary with `CGO_ENABLED=0`, and it reached the socket |
| `STATICCHECK` | PASS | staticcheck 2025.1.1 clean |

## What we learned

- **Socket access works with a systemd drop-in.** `podman.socket` listens on `/run/wsm-podman/<user>/podman.sock`, mode 0660, group `wsm`, in a setgid directory `drwxr-s---`. See `docs/adr/0002-podman-socket-access.md`.
- **The thin REST client is enough for this spike.** With only the standard library plus `coder/websocket`, it covers pod creation, hardening, exec with TTY and resize against Podman 5.7.0.
- **The bindings build, but they are heavy.** The probe is 36.4 MB with 120 listed dependencies, including `github.com/mattn/go-sqlite3` in the module list, although the binary is static. It used bindings v5.4.2 against a 5.7.0 server, and it only exercised a basic connection, not the full API surface.

## Surprises and open questions

- The Go version on the machine (1.26.8) differs from the local one (1.27.3).
- The socket is reachable by every member of group `wsm`, which is intended to be only the manager. The tests did not cover additional members.

## Decisions

- Socket access: `docs/adr/0002-podman-socket-access.md`.
- Bindings or REST client: the owner chose the thin REST client; see `docs/adr/0003-podman-rest-client.md`.
