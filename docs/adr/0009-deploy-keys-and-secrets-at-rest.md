# 0009: Deploy keys and secrets at rest

## Status

Accepted.

## Context

M2b (plan section 5) gives a project a git repository. The manager needs a credential to read it. Requirements "Git, review en signing" and assumptions 5 and 6 say the credential is a deploy key per project, and that no credential ever enters a sandbox. Plan section 3.12 puts the key in the `projects` table (`deploy_key_pub`, `deploy_key_priv_enc`) and says secrets are stored encrypted, with a key file in `/var/lib/wsm` with mode 0600.

HTTPS remotes would need tokens and their own secret handling. Phase 2 brings a secret store for sandboxes; this ADR is not that.

## Decision

1. **Only SSH remotes.** A repository URL is `ssh://user@host[:port]/path` or the scp-like `user@host:path`. Both forms need a user. wsm validates the URL strictly (`gitmgr.ValidRepoURL`): no part starts with a dash, there are no spaces or control characters, no `..` path segments, and no other transports (`file://`, `ext::`, `http(s)://`, `git://`, local paths). The base branch, `main` by default, is a strict subset of what `git check-ref-format` allows.
2. **One ed25519 deploy key per project, made by wsm.** `project create --repo` generates the key with the standard library (`crypto/ed25519`) and writes it in OpenSSH's own private key format, without a passphrase. `ssh-keygen -y` reads it and returns the same public key (`TestDeployKeyOpenSSH`). The comment is `wsm-<project slug>`. `wsm project key SLUG` shows the public part, which the owner adds to the Git host as a read-only deploy key. `project create` does not contact the origin; the first contact is the first seed (ADR 0010).
3. **The private key is stored only sealed.** It is sealed with AES-256-GCM (`internal/secret`) under a 32-byte key from `/var/lib/wsm/secret.key`. A sealed value is a random 12-byte nonce followed by the ciphertext and tag. The project ID is the additional data, so a sealed key does not open for another project. The store's scans never return the sealed key; only `Store.DeployKeySealed` reads it, for a seed. No API response, log line, job error or CLI output contains it.
4. **The key file.** `wsm serve` creates `secret.key` on its first start with `O_EXCL` and mode 0600, in the 0700 state directory. On later starts the file must be a regular file (not a symlink) of exactly 32 bytes with mode 0600, or `wsm serve` refuses to start. It never rewrites an existing file.
5. **The private key on disk.** For each git command that talks to the origin, wsm writes the key to a 0600 file in a new 0700 directory under `/var/lib/wsm/git` and removes the directory as soon as the command ends. Directories left behind by a crash are removed when the manager starts. Only paths that wsm derives itself go into `GIT_SSH_COMMAND`.
6. **Host keys.** ssh runs with `StrictHostKeyChecking=accept-new` and a known_hosts file per project next to its mirror. The first contact records the host key; a changed host key makes later fetches fail. Deleting the project removes the known_hosts file with the mirror.

## Consequences

- Anyone who can read both the database and `secret.key` can recover the deploy keys. Both live in `/var/lib/wsm`, which only `wsm` can read (`TestManagerStateNotReachable`). The encryption protects copies of the database, such as backups, that do not include the key file.
- Losing `secret.key` makes every deploy key unreadable. The owner then has to re-create the projects or, later, rotate their keys. Key rotation is follow-up work.
- `accept-new` trusts the host key on first use. An attacker who controls the network path at the first seed could plant its own key. Pinning host keys at `project create` is possible follow-up work.
- Private registries and HTTPS remotes stay out of scope until phase 2 brings a secret store.
