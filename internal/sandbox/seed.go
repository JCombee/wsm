package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/gitmgr"
	"github.com/JCombee/wsm/internal/podman"
)

// seedBundle is where the bundle lands in the workspace container, only
// for the duration of the seed.
const seedBundle = "/tmp/.wsm-seed.bundle"

// seedTimeout bounds the whole seed: mirror update, bundle and execs.
const seedTimeout = 20 * time.Minute

// maxSeedMessage caps the sandbox output quoted in a seed error. The
// output comes from the sandbox and is untrusted (invariant 8).
const maxSeedMessage = 4 << 10

// readyTimeout is how long the seed waits for exec to work after the pod
// started.
const readyTimeout = 30 * time.Second

// seedCommands returns the execs that put the bundle's base branch into
// /workspace on the sandbox's own branch, in order. The first one gets the
// bundle on stdin. None runs through a shell.
func seedCommands(baseBranch, branch string) [][]string {
	return [][]string{
		{"dd", "of=" + seedBundle, "status=none"},
		{"git", "clone", "--quiet", "--branch", baseBranch, seedBundle, WorkspaceDir},
		{"git", "-C", WorkspaceDir, "switch", "--quiet", "--create", branch},
		{"git", "-C", WorkspaceDir, "remote", "remove", "origin"},
		{"rm", "-f", seedBundle},
	}
}

// seed brings the project's mirror up to date and moves a bundle of the
// base branch into the sandbox, where it becomes /workspace on branch
// agent/<slug> without a remote (ADR 0010). git runs on the host only in
// the mirror; in /workspace it runs inside the container, as the agent's
// user. wsm never deletes files in /workspace.
func (m *Manager) seed(ctx context.Context, pc *podman.Client, p db.Project, sb db.Sandbox) error {
	if m.Mirrors == nil || m.Secrets == nil {
		return errors.New("seed: no git mirrors configured")
	}
	if sb.Branch == "" {
		return errors.New("seed: sandbox has no branch")
	}
	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()

	sealed, err := m.Store.DeployKeySealed(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	key, err := m.Secrets.Open(sealed, []byte(p.ID))
	if err != nil {
		return fmt.Errorf("seed: deploy key of project %s: %w", p.Slug, err)
	}
	err = m.Mirrors.Update(ctx, gitmgr.Repo{ProjectID: p.ID, URL: p.RepoURL, Key: key})
	clear(key)
	if err != nil {
		return fmt.Errorf("seed: update mirror of %s: %w", p.RepoURL, err)
	}

	ctr := WorkContainer(sb.ID)
	if err := waitForExec(ctx, pc, ctr); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	cmds := seedCommands(p.BaseBranch, sb.Branch)
	if err := m.sendBundle(ctx, pc, ctr, p, cmds[0]); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	for _, cmd := range cmds[1:] {
		if err := runChecked(ctx, pc, ctr, nil, cmd); err != nil {
			if _, rerr := pc.Run(context.WithoutCancel(ctx), ctr, "rm", "-f", seedBundle); rerr != nil {
				m.logf("seed %s/%s: remove bundle: %v", p.Slug, sb.Slug, rerr)
			}
			if cmd[1] == "clone" && strings.Contains(err.Error(), "not an empty directory") {
				return fmt.Errorf("seed: %w; wsm does not delete files in %s: delete and re-create the sandbox", err, WorkspaceDir)
			}
			return fmt.Errorf("seed: %w", err)
		}
	}
	if err := m.Store.MarkSandboxSeeded(ctx, sb.ID); err != nil {
		return err
	}
	m.logf("sandbox %s/%s seeded with %s on %s", p.Slug, sb.Slug, p.BaseBranch, sb.Branch)
	return nil
}

// sendBundle streams a bundle of the base branch from the mirror into the
// container, as stdin of cmd.
func (m *Manager) sendBundle(ctx context.Context, pc *podman.Client, ctr string, p db.Project, cmd []string) error {
	pr, pw := io.Pipe()
	bundleErr := make(chan error, 1)
	go func() {
		err := m.Mirrors.Bundle(ctx, p.ID, p.BaseBranch, pw)
		pw.CloseWithError(err)
		bundleErr <- err
	}()
	err := runChecked(ctx, pc, ctr, pr, cmd)
	// Unblock the bundle writer if the exec stopped reading early.
	pr.CloseWithError(errors.New("exec ended"))
	if berr := <-bundleErr; berr != nil {
		return fmt.Errorf("bundle of %s: %w", p.BaseBranch, berr)
	}
	return err
}

// runChecked runs cmd in the container and fails on a non-zero exit code,
// quoting the sanitized tail of its stderr.
func runChecked(ctx context.Context, pc *podman.Client, ctr string, stdin io.Reader, cmd []string) error {
	res, err := pc.RunInput(ctx, ctr, stdin, cmd...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s %s: exit code %d: %s", cmd[0], cmd[1], res.ExitCode, quote(res.Stderr))
	}
	return nil
}

// quote returns the sanitized last maxSeedMessage bytes of untrusted output.
func quote(s string) string {
	if len(s) > maxSeedMessage {
		s = s[len(s)-maxSeedMessage:]
	}
	return gitmgr.Sanitize(s)
}

// waitForExec waits until an exec works in a container that just started.
func waitForExec(ctx context.Context, pc *podman.Client, ctr string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	for {
		res, err := pc.Run(ctx, ctr, "true")
		if err == nil && res.ExitCode == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			if err == nil {
				err = fmt.Errorf("exit code %d", res.ExitCode)
			}
			return fmt.Errorf("container %s is not ready for exec: %w", ctr, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
