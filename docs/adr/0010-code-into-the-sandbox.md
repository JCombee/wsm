# 0010: Code into the sandbox

## Status

Accepted.

## Context

Plan sections 3.4 and 3.5 say code crosses the sandbox boundary only as git bundles, through exec and stdin, and that a sandbox works on its own branch `agent/<sandbox>`. CLAUDE.md invariant 3 says the manager never runs git in a directory an agent controls, and that git on the mirrors runs with hooks and fsmonitor disabled. Invariant 8 says everything that comes out of a sandbox is untrusted.

The preflight of M2b showed on the VM that a bundle reaches the workspace container through the libpod exec API with stdin, and that git and ssh work as `wsm` under the sandboxing of `wsm.service` with a fully explicit environment.

## Decision

1. **A bare mirror per project.** It lives in `/var/lib/wsm/git/<project id>.git`, inside the manager's 0700 state directory. The first update is `git clone --mirror`, into a temporary name that is renamed when the clone succeeds. Later updates are `git fetch --prune` of `refs/heads/*` and `refs/tags/*`, with the URL from the database rather than from the mirror's config. Mirrors are only updated when a sandbox is seeded, not periodically. Deleting a project removes its mirror and known_hosts.
2. **git on the host runs locked down.** Every git command:
   - runs without a shell, with fixed arguments and `--` before the URL;
   - gets `-c core.hooksPath=/dev/null -c core.fsmonitor=false`;
   - gets a complete environment and inherits nothing: `PATH=/usr/bin:/bin`, `HOME=/var/lib/wsm`, `GIT_ALLOW_PROTOCOL=ssh`, `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_SSH_COMMAND`;
   - uses `ssh -F /dev/null -i <key file> -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=30 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=<mirror>.known_hosts` (ADR 0009);
   - has a timeout of 10 minutes;
   - quotes at most the last 4 KiB of its stderr in an error, with control characters replaced, as in ADR 0008 point 7. That output includes messages from the remote.
3. **Seeding happens on the first start.** Exec needs a running container, so `sandbox create` does not seed; the first `sandbox start` does. The manager updates the mirror, waits until exec works, and streams `git bundle create - refs/heads/<base>` from the mirror into the container. The sandbox's `seeded_at` is set afterwards, and later starts do not seed again.
4. **Inside the container, as the agent's user.** The seed is five execs in the workspace container, each with a fixed argv and no `sh -c`, running as the container user 1000:
   1. `dd of=/tmp/.wsm-seed.bundle status=none`, with the bundle on stdin. The end of stdin is signalled by half-closing the hijacked API connection;
   2. `git clone --quiet --branch <base> /tmp/.wsm-seed.bundle /workspace`;
   3. `git -C /workspace switch --quiet --create agent/<slug>`;
   4. `git -C /workspace remote remove origin`;
   5. `rm -f /tmp/.wsm-seed.bundle`.

   git on the host never touches `/workspace` or the code volume. The sandbox ends up with the history of the base branch, its own branch, no remote and no credentials.
5. **Failure.** If any step fails, `sandbox start` fails with the reason, the pod is stopped and the sandbox gets status `failed`. The next start tries again. Exec output and exit codes come from the sandbox: output is capped at 1 MiB per exec, and an error quotes at most 4 KiB of sanitized stderr.
6. **wsm never deletes files in `/workspace`.** If `/workspace` is not empty, for example after a seed that failed half-way, `git clone` fails and the error tells the owner to delete and re-create the sandbox.
7. **`WSM_BRANCH`.** A sandbox in a project with a repository has `branch = agent/<slug>`, and its workspace container gets `WSM_BRANCH=agent/<slug>` (plan section 3.4).

## Consequences

- An agent can rewrite its branch and history freely. Getting code back out (bundles from the sandbox, re-signing, review and push) is phase 2.
- A seed can take as long as a fetch of the whole repository. The manager runs one state-changing operation at a time, so a large first fetch holds up other operations, at most for the timeouts.
- Sandboxes of projects with a repository need `git` in their image, which plan section 3.8 already requires.
- Isolation tests `TestCodeArrivesOnAgentBranch`, `TestNoCredentialsInSandbox`, `TestMirrorIgnoresHooks` and the extended `TestManagerStateNotReachable` cover this decision on the VM, with a local SSH origin that runs `git-shell`.
