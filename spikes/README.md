# Spikes

Throwaway experiments that settle the riskiest questions before M1 (see
`docs/plan/bouwplan-fase-1.md`, section 5). Spike code never becomes part of
the main module.

| Spike | What it proves | Runner |
|---|---|---|
| `01-network` | nftables per project UID with rootless pasta pods, MariaDB on `wsm0` | `01-network/run.sh` |
| `02-podman-api` | the manager drives a project user's Podman over its socket; exec with TTY to WebSocket; Podman bindings without cgo | `02-podman-api/run.sh` |
| `03-mcp` | an MCP server (go-sdk) as `wsm` on `10.255.0.1:7443` with a bearer token per sandbox; Claude Code and Codex call a tool from a real sandbox via a mock model. Outcome: go for M3 (ADR 0011) | `03-mcp/run.sh` (needs an installed wsm) |
| `04-passkeys` | go-webauthn with a strict RP ID, `__Host-` cookies and preview access, in headless Chromium with a virtual authenticator. Outcome: go for M4 (ADR 0016, 0017) | `04-passkeys/run.sh` |
| `05-paperclip-provider` | a Paperclip 2026.1117.0 sandbox-provider plugin (TypeScript) that runs a `claude_local` agent with the mock model in wsm sandboxes: leases through the management API, exec, native sync with a strict tar reader, and both bridge forms over exec without a network path. Outcome: go for S2 and S3 (`FINDINGS.md`) | `05-paperclip-provider/vm/run.sh all` (needs the wsm binaries) |

The scripts install packages, create users, change nftables and start
containers. **Run them only on a disposable VM**, never on a machine with real
client data. Both scripts refuse to run unless `/etc/wsm-spike-disposable`
exists (exit 3) and they run as root (exit 4).

## Results

Runs are stored in `spikes/runs/<run-id>/` (gitignored, because they hold server
addresses). `spikes/runs/LATEST` names the newest run. Each run has:

- `run.txt`: commit, server details and timestamps;
- `SUMMARY.txt`: every criterion with PASS or FAIL;
- `status.txt`: the exit code per spike;
- `<spike>/console.log` and `<spike>/results/`: the raw output.

The owner's `wsm-test-server` script, kept outside this repository, does the
steps below over SSH and stores the results here. It tests a commit, so commit
changes to the spikes first. The manual steps remain as a fallback.

## 1. Create the VM

- Debian 13 (trixie), amd64, minimal install with SSH server.
- At least 2 vCPU, 4 GB RAM and 20 GB disk.
- Outbound internet access (apt, `docker.io`, `proxy.golang.org`).
- No shared folders or other mounts from your workstation.

Any hypervisor works (libvirt/virt-manager, Proxmox) and so does a cloud VM;
both are fine. On a cloud VM the metadata address is real, so `NET-METADATA`
proves the metadata service is unreachable. On a local VM nothing listens
there, so `NET-METADATA` only proves that the rule blocks the address.

## 2. Copy the spikes to the VM

On your workstation, from the repository root:

```sh
git archive --format=tar.gz -o wsm-spikes.tar.gz HEAD spikes/
scp wsm-spikes.tar.gz root@<vm>:/root/
```

On the VM, as root:

```sh
mkdir -p /root/wsm && tar -xzf /root/wsm-spikes.tar.gz -C /root/wsm
```

## 3. Mark the VM as disposable

On the VM, as root. This marker is the only thing that lets the scripts run:

```sh
touch /etc/wsm-spike-disposable
```

## 4. Run the spikes

On the VM, as root, one after the other:

```sh
cd /root/wsm
bash spikes/01-network/run.sh 2>&1 | tee /root/spike01.log
bash spikes/02-podman-api/run.sh 2>&1 | tee /root/spike02.log
```

Each script takes a few minutes, mostly for apt and image pulls. A failing
test does not stop the script. The exit code is 0 when every criterion
passed and 1 otherwise.

The scripts can be rerun; they remove their own pods, containers and
nftables table first. They leave the final state running (pods, MariaDB, the
`inet wsm` table) so you can inspect it.

## 5. Check the results

Each spike writes to `spikes/<spike>/results/`:

- `summary.txt`: one line per criterion, `ID<TAB>PASS|FAIL<TAB>detail`, plus
  `INFO` lines;
- `<ID>.log`: raw output per test;
- versions, `podman info`, `nft list ruleset` and other diagnostics.

Quick look:

```sh
cat spikes/*/results/summary.txt
grep -h $'\tFAIL\t' spikes/*/results/summary.txt
```

No passwords end up in `results/`. MariaDB passwords are generated per run
and only live in the environment of the processes that need them.

## 6. Hand back the results

```sh
cd /root/wsm
tar -czf /root/wsm-spike-results.tar.gz spikes/*/results /root/spike01.log /root/spike02.log
```

Copy the tarball back (`scp root@<vm>:/root/wsm-spike-results.tar.gz .`) and
hand it over. A follow-up session fills in `FINDINGS.md` and writes the ADRs.
Afterwards, delete the VM.

## FINDINGS.md template

Each spike has a `FINDINGS.md` that starts as "NOG NIET UITGEVOERD". After a
run it gets these sections:

```markdown
# Findings: spike NN <name>

- **Run:** date, VM (OS, kernel, vCPU/RAM), versions from results/
- **Outcome:** go / no-go for the milestone this spike gates

## Criteria
| ID | Result | Evidence |
|---|---|---|

## What we learned
## Surprises and open questions
## Decisions (each one becomes an ADR in docs/adr/)
```
