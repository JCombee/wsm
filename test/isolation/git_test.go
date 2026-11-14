//go:build isolation

package isolation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/gitmgr"
	"github.com/JCombee/wsm/internal/sandbox"
	"github.com/JCombee/wsm/internal/secret"
)

// M2b: a project's code comes from its origin over SSH with a deploy key,
// through a mirror on the host, into the sandbox as a bundle (ADR 0009 and
// ADR 0010).

// The test origin: a local user with git-shell, reached over SSH on the
// loopback address. The wsm egress rules filter only project UIDs, so
// wsm itself reaches it.
const (
	originUser = "wsm-test-origin"
	originHome = "/home/" + originUser
	originRepo = originHome + "/app.git"
	originURL  = originUser + "@127.0.0.1:app.git"
	gitSlug    = "iso-h"
)

// gitFixture is the project with a repository that the M2b tests share.
type gitFixture struct {
	p          project
	baseCommit string // main on the origin when the sandbox was seeded
	work       string // root's working copy that feeds the origin
}

var (
	gitOnce sync.Once
	gitFix  *gitFixture
	gitErr  error
)

// rootGit runs git as root for the fixtures, without any system or user
// config.
func rootGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-i", "PATH=/usr/bin:/bin", "HOME=" + dir, "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1", "git", "-C", dir, "-c", "user.name=wsm test", "-c", "user.email=test@wsm.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)
	out, err := run(ctx, "env", full...)
	return strings.TrimSpace(out), err
}

// asOrigin runs a command as the origin user, from "/".
func asOrigin(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-u", originUser, "--", "env", "-i", "PATH=/usr/bin:/bin", "HOME=" + originHome,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}, args...)
	return run(ctx, "runuser", full...)
}

// commitAndPublish commits a file in root's working copy and moves the
// origin's main to it through a bundle, which the origin user fetches. A
// direct clone or push would cross owners and trip safe.directory.
func commitAndPublish(ctx context.Context, work, name string) (string, error) {
	if err := os.WriteFile(filepath.Join(work, name), []byte(name+"\n"), 0o644); err != nil {
		return "", err
	}
	if _, err := rootGit(ctx, work, "add", name); err != nil {
		return "", err
	}
	if _, err := rootGit(ctx, work, "commit", "--quiet", "-m", name); err != nil {
		return "", err
	}
	head, err := rootGit(ctx, work, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	bundle := filepath.Join(s.tmp, "origin-"+name+".bundle")
	if _, err := rootGit(ctx, work, "bundle", "create", "--quiet", bundle, "main"); err != nil {
		return "", err
	}
	if err := os.Chmod(bundle, 0o644); err != nil {
		return "", err
	}
	if _, err := os.Stat(originRepo); err != nil {
		_, err = asOrigin(ctx, "git", "clone", "--quiet", "--bare", bundle, originRepo)
		return head, err
	}
	_, err = asOrigin(ctx, "git", "-C", originRepo, "fetch", "--quiet", bundle, "main:main")
	return head, err
}

// removeOrigin deletes the origin user and its home.
func removeOrigin(ctx context.Context) {
	run(ctx, "pkill", "-u", originUser)
	run(ctx, "userdel", "--remove", originUser)
}

// setupGit creates the origin, the project with its repository and one
// running sandbox, seeded on its first start.
func setupGit(ctx context.Context) (*gitFixture, error) {
	removeOrigin(ctx)
	// A system user: a regular useradd takes the highest UID plus one,
	// which lands in the project range after the first projects.
	if _, err := run(ctx, "useradd", "--system", "--create-home", "--home-dir", originHome,
		"--shell", "/usr/bin/git-shell", originUser); err != nil {
		return nil, err
	}
	// A usable account without a password: "!" would lock it for sshd.
	if _, err := run(ctx, "usermod", "--password", "*", originUser); err != nil {
		return nil, err
	}
	f := &gitFixture{work: filepath.Join(s.tmp, "origin-work")}
	if err := os.Mkdir(f.work, 0o755); err != nil {
		return nil, err
	}
	if _, err := rootGit(ctx, f.work, "init", "--quiet"); err != nil {
		return nil, err
	}
	var err error
	if f.baseCommit, err = commitAndPublish(ctx, f.work, "README"); err != nil {
		return nil, err
	}

	if f.p, err = createProjectFrom(ctx, s, gitSlug, "--image", testImage, "--repo", originURL); err != nil {
		return nil, err
	}
	out, err := wsm(ctx, "project", "key", gitSlug)
	if err != nil {
		return nil, err
	}
	pub := strings.TrimSpace(out)
	if !regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/=]+ wsm-` + gitSlug + `$`).MatchString(pub) {
		return nil, fmt.Errorf("wsm project key: unexpected output %q", out)
	}
	sshDir := filepath.Join(originHome, ".ssh")
	if _, err := asOrigin(ctx, "mkdir", "-m", "0700", "-p", sshDir); err != nil {
		return nil, err
	}
	keys := filepath.Join(sshDir, "authorized_keys")
	if err := os.WriteFile(keys, []byte("restrict "+pub+"\n"), 0o600); err != nil {
		return nil, err
	}
	if _, err := run(ctx, "chown", originUser+":", keys); err != nil {
		return nil, err
	}

	if err := shareImage(ctx, s, s.a, f.p); err != nil {
		return nil, err
	}
	if err := createSandbox(ctx, s, &f.p); err != nil {
		return nil, err
	}
	return f, nil
}

// gitProject returns the shared fixture, setting it up on first use.
func gitProject(t *testing.T) *gitFixture {
	t.Helper()
	gitOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		gitFix, gitErr = setupGit(ctx)
	})
	if gitErr != nil {
		t.Fatalf("git fixture: %v", gitErr)
	}
	return gitFix
}

func TestCodeArrivesOnAgentBranch(t *testing.T) {
	ctx := ctxT(t)
	f := gitProject(t)
	out, err := inSandbox(ctx, f.p, "git", "-C", "/workspace", "branch", "--show-current")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != "agent/main" {
		t.Errorf("/workspace is on %q, want agent/main", got)
	}
	out, err = inSandbox(ctx, f.p, "git", "-C", "/workspace", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != f.baseCommit {
		t.Errorf("HEAD = %s, want the origin's main %s", got, f.baseCommit)
	}
	out, err = inSandbox(ctx, f.p, "cat", "/workspace/README")
	if err != nil || strings.TrimSpace(out) != "README" {
		t.Errorf("/workspace/README: %q, %v", out, err)
	}
	out, err = inSandbox(ctx, f.p, "printenv", "WSM_BRANCH")
	if err != nil || strings.TrimSpace(out) != "agent/main" {
		t.Errorf("WSM_BRANCH = %q, %v", out, err)
	}
	// The agent's user owns the checkout.
	out, err = inSandbox(ctx, f.p, "stat", "-c", "%u:%g", "/workspace/.git")
	if err != nil || strings.TrimSpace(out) != "1000:1000" {
		t.Errorf("/workspace/.git owner %q, %v", out, err)
	}
	sb, ok, err := findSandbox(ctx, gitSlug, "main")
	if err != nil || !ok || sb.Branch != "agent/main" {
		t.Errorf("API sandbox %+v, %v, %v", sb, ok, err)
	}
}

func TestNoCredentialsInSandbox(t *testing.T) {
	ctx := ctxT(t)
	f := gitProject(t)
	// No private key anywhere in the container's files. grep exits 1
	// when nothing matches.
	// -s hides errors for files the agent cannot read.
	out, err := inSandbox(ctx, f.p, "timeout", "120", "grep", "-rIls", "-e", "PRIVATE KEY",
		"/workspace", "/home/agent", "/tmp", "/etc", "/root", "/var")
	if err == nil || strings.TrimSpace(out) != "" {
		t.Errorf("private key material in the sandbox: %q", out)
	}
	// Control: grep finds what is there.
	if _, err := inSandbox(ctx, f.p, "grep", "-rIl", "README", "/workspace"); err != nil {
		t.Errorf("control: grep found nothing in /workspace: %v", err)
	}
	env, err := inSandbox(ctx, f.p, "env")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"PRIVATE KEY", "GIT_SSH", "SSH_AUTH_SOCK", "BEGIN OPENSSH"} {
		if strings.Contains(env, bad) {
			t.Errorf("the sandbox environment contains %q", bad)
		}
	}
	out, err = inSandbox(ctx, f.p, "git", "-C", "/workspace", "remote", "-v")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("remotes in /workspace: %q", out)
	}
	if out, err := inSandbox(ctx, f.p, "test", "-e", "/tmp/.wsm-seed.bundle"); err == nil {
		t.Errorf("the seed bundle is still in the sandbox: %s", out)
	}
	if out, err := inSandbox(ctx, f.p, "test", "-e", "/home/agent/.ssh"); err == nil {
		t.Errorf("/home/agent/.ssh exists in the sandbox: %s", out)
	}
	// The project's own user reaches neither its mirror nor the key file.
	mirror := filepath.Join(gitmgr.DefaultDir, f.p.id+".git")
	for _, spec := range []string{"readdir:" + mirror, "readfile:" + filepath.Join(mirror, "config"),
		"readfile:" + secret.DefaultKeyFile} {
		if ok, out := probeAs(ctx, f.p.user, spec); ok {
			t.Errorf("%s: %s", f.p.user, out)
		}
	}
}

func TestMirrorIgnoresHooks(t *testing.T) {
	ctx := ctxT(t)
	f := gitProject(t)
	mirror := filepath.Join(gitmgr.DefaultDir, f.p.id+".git")
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Fatalf("mirror %s: %v", mirror, err)
	}
	// The marker is in the mirror directory, which wsm.service can write.
	marker := filepath.Join(gitmgr.DefaultDir, "hook-ran")
	os.Remove(marker)
	hook := filepath.Join(mirror, "hooks", "reference-transaction")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, "chown", "wsm:wsm", hook); err != nil {
		t.Fatal(err)
	}
	// The mirror's own config points at the hooks too; the command line
	// must win.
	cfg, err := os.OpenFile(filepath.Join(mirror, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(cfg, "[core]\n\thooksPath = %s\n", filepath.Dir(hook))
	cfg.Close()

	// Control: the hook works when it runs, as wsm.
	if _, err := run(ctx, "runuser", "-u", "wsm", "--", hook); err != nil {
		t.Fatalf("control: run the hook: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the hook left no marker: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	// A new commit on the origin, then a new sandbox: its seed fetches into
	// the mirror, which is a reference transaction.
	head, err := commitAndPublish(ctx, f.work, "second")
	if err != nil {
		t.Fatal(err)
	}
	second := project{slug: f.p.slug, id: f.p.id, user: f.p.user, uid: f.p.uid}
	target := gitSlug + "/second"
	if _, err := wsm(ctx, "sandbox", "create", target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wsm(context.Background(), "sandbox", "delete", target) })
	if _, err := wsm(ctx, "sandbox", "start", target); err != nil {
		t.Fatal(err)
	}
	sb, ok, err := findSandbox(ctx, gitSlug, "second")
	if err != nil || !ok {
		t.Fatalf("sandbox second: %v %v", ok, err)
	}
	second.container = sandbox.WorkContainer(sb.ID)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the reference-transaction hook in the mirror ran during the seed")
	}
	out, err := inSandbox(ctx, second, "git", "-C", "/workspace", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != head {
		t.Errorf("second sandbox HEAD = %s, want the new origin main %s: the mirror was not fetched", got, head)
	}
	out, err = inSandbox(ctx, second, "git", "-C", "/workspace", "branch", "--show-current")
	if err != nil || strings.TrimSpace(out) != "agent/second" {
		t.Errorf("second sandbox branch %q, %v", out, err)
	}
}

// TestZZDeleteRemovesMirror runs after the other M2b tests: deleting the
// project removes its mirror and known_hosts.
func TestZZDeleteRemovesMirror(t *testing.T) {
	ctx := ctxT(t)
	f := gitProject(t)
	mirror := filepath.Join(gitmgr.DefaultDir, f.p.id+".git")
	knownHosts := filepath.Join(gitmgr.DefaultDir, f.p.id+".known_hosts")
	for _, p := range []string{mirror, knownHosts} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("control: %v", err)
		}
	}
	if _, err := wsm(ctx, "project", "delete", gitSlug); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{mirror, knownHosts} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s still exists after project delete", p)
		}
	}
	entries, _ := os.ReadDir(gitmgr.DefaultDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "key-") {
			t.Errorf("deploy key directory left: %s", e.Name())
		}
	}
}
