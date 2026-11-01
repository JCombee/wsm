NOG NIET UITGEVOERD
===================

# Findings: spike 02 Podman API

Not run yet. After a run on a disposable VM, replace this file using the
template in `spikes/README.md`, with evidence from `results/`.

## Criteria to fill in

| ID | What it shows | Result | Evidence |
|---|---|---|---|
| `POD-SOCKET-ACCESS` | `wsm` reaches `wsm-p-a`'s socket through group `wsm`; `wsm-p-b` does not | | |
| `POD-USERNS` | pod with `keep-id:uid=1000,gid=1000`; uid 1000 inside is the project user | | |
| `POD-HARDENING` | `NoNewPrivs=1`, `Seccomp=2`, `CapEff=0`, uid 1000, cpu/memory/pids limits, no mounts | | |
| `POD-EXEC-WS` | exec output comes back over the WebSocket | | |
| `POD-RESIZE` | a resize message is visible in `stty size` | | |
| `POD-BINDINGS-CGO0` | official bindings build static without cgo (FAIL means: own REST client) | | |
| `GO-TEST` | unit tests and `go vet` pass on the VM | | |
| `STATICCHECK` | `staticcheck` is clean | | |

## Questions to answer

- Is the socket in a setgid directory under `/run/wsm-podman/` the approach
  for M1? The socket drop-in lives in the project user's home, which that
  user can change; does it belong in `/etc/systemd/user/` instead?
- `--init` makes Podman bind-mount catatonit into the container; does that
  show up in `mountinfo` (see `podmanapi.log`), and is it acceptable next to
  invariant 1?
- Does Podman answer exec start with 101 or 200, and does resize work right
  after start?
- Binary size and dependency count of the bindings probe
  (`bindings-deps.txt`) against the own REST client.
