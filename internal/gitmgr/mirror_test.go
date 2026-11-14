package gitmgr

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testGit runs git for test fixtures, without the user's or the system's
// config: the owner's global config signs commits through a signer the
// sandbox blocks.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=wsm test", "-c", "user.email=test@wsm.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newOrigin makes a repository with one commit on main and returns its
// file:// URL and directory.
func newOrigin(t *testing.T) (url, dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "origin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "init", "--quiet")
	commit(t, dir, "one")
	return "file://" + dir, dir
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, msg+".txt"), []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "add", msg+".txt")
	testGit(t, dir, "commit", "--quiet", "-m", msg)
	return testGit(t, dir, "rev-parse", "HEAD")
}

// testMirrors returns mirrors that also allow file://, for local origins.
// Production code allows ssh only.
func testMirrors(t *testing.T, git string) *Mirrors {
	t.Helper()
	root := t.TempDir()
	m, err := NewMirrors(filepath.Join(root, "git"), root, git)
	if err != nil {
		t.Fatal(err)
	}
	m.protocols = "ssh:file"
	m.validURL = func(string) error { return nil }
	return m
}

const testProject = "abcdefgh2345"

var testKey = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnot a real key\n-----END OPENSSH PRIVATE KEY-----\n")

func TestMirrorDisablesHooks(t *testing.T) {
	ctx := context.Background()
	url, origin := newOrigin(t)
	m := testMirrors(t, DefaultGit)
	r := Repo{ProjectID: testProject, URL: url, Key: testKey}
	if err := m.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	mirror, _ := m.Path(testProject)

	// A hook in the mirror that would leave a marker when it runs.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(mirror, "hooks", "reference-transaction")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho ran >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := commit(t, origin, "two")
	if err := m.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the reference-transaction hook ran during fetch")
	}
	got := testGit(t, mirror, "--git-dir="+mirror, "rev-parse", "refs/heads/main")
	if got != head {
		t.Errorf("mirror main = %s, want %s", got, head)
	}
	var bundle bytes.Buffer
	if err := m.Bundle(ctx, testProject, "main", &bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran during bundle create")
	}

	// Control: the same fetch without core.hooksPath runs the hook.
	commit(t, origin, "three")
	testGit(t, mirror, "--git-dir="+mirror, "-c", "protocol.file.allow=always", "fetch", "--quiet", "--", url, "+refs/heads/*:refs/heads/*")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the hook did not run without core.hooksPath: %v", err)
	}
}

func TestMirrorBundleAndRemove(t *testing.T) {
	ctx := context.Background()
	url, origin := newOrigin(t)
	head := testGit(t, origin, "rev-parse", "HEAD")
	m := testMirrors(t, DefaultGit)
	r := Repo{ProjectID: testProject, URL: url, Key: testKey}
	if err := m.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if err := m.Bundle(ctx, testProject, "main", &bundle); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(path, bundle.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := testGit(t, t.TempDir(), "bundle", "list-heads", path); !strings.Contains(out, head+" refs/heads/main") {
		t.Errorf("bundle heads %q, want %s refs/heads/main", out, head)
	}

	if err := m.Bundle(ctx, testProject, "missing", &bundle); err == nil {
		t.Error("bundle of a missing branch succeeded")
	}
	if err := m.Bundle(ctx, testProject, "--all", &bundle); err == nil {
		t.Error("bundle accepted an option as branch")
	}
	if err := m.Update(ctx, Repo{ProjectID: "../x", URL: url, Key: testKey}); err == nil {
		t.Error("Update accepted an invalid project id")
	}
	if err := m.Update(ctx, Repo{ProjectID: "other2345abc", URL: url}); err == nil {
		t.Error("Update without a key succeeded")
	}

	// A failed clone leaves no mirror behind.
	bad := Repo{ProjectID: "failed234567", URL: "file:///nonexistent/repo", Key: testKey}
	if err := m.Update(ctx, bad); err == nil {
		t.Error("clone of a missing origin succeeded")
	}
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "failed") || strings.HasPrefix(e.Name(), "key-") {
			t.Errorf("left behind: %s", e.Name())
		}
	}

	mirror, _ := m.Path(testProject)
	if err := os.WriteFile(filepath.Join(m.dir, testProject+".known_hosts"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(testProject); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{mirror, filepath.Join(m.dir, testProject+".known_hosts")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left after Remove: %v", p, err)
		}
	}
	if err := m.Remove(testProject); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

func TestMirrorProductionAllowsOnlySSH(t *testing.T) {
	root := t.TempDir()
	m, err := NewMirrors(filepath.Join(root, "git"), root, DefaultGit)
	if err != nil {
		t.Fatal(err)
	}
	url, _ := newOrigin(t)
	// The URL check refuses file:// first.
	if err := m.Update(context.Background(), Repo{ProjectID: testProject, URL: url, Key: testKey}); err == nil {
		t.Fatal("production mirrors cloned a file:// URL")
	}
	// Without the URL check, git itself refuses the protocol.
	m.validURL = func(string) error { return nil }
	err = m.Update(context.Background(), Repo{ProjectID: testProject, URL: url, Key: testKey})
	if err == nil || !strings.Contains(err.Error(), "transport 'file' not allowed") {
		t.Fatalf("file:// with GIT_ALLOW_PROTOCOL=ssh: %v", err)
	}
	if _, err := NewMirrors(filepath.Join(root, "a b"), root, DefaultGit); err == nil {
		t.Error("NewMirrors accepted a path with a space")
	}
}

// fakeGit logs its arguments and environment, and the mode of the key
// file named in GIT_SSH_COMMAND, then runs the real git.
const fakeGit = `#!/bin/sh
{
  echo "=== call"
  for a in "$@"; do echo "arg $a"; done
  env | sort | sed 's/^/env /'
  key=$(echo "$GIT_SSH_COMMAND" | sed -n 's/.* -i \([^ ]*\) .*/\1/p')
  if [ -n "$key" ]; then echo "keymode $(stat -c %a "$key")"; echo "keydir $(stat -c %a "$(dirname "$key")")"; fi
} >> LOG
exec /usr/bin/git "$@"
`

func TestGitEnvironment(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	git := filepath.Join(dir, "git")
	if err := os.WriteFile(git, []byte(strings.ReplaceAll(fakeGit, "LOG", logFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	url, origin := newOrigin(t)
	m := testMirrors(t, git)
	r := Repo{ProjectID: testProject, URL: url, Key: testKey}
	if err := m.Update(ctx, r); err != nil { // clone
		t.Fatal(err)
	}
	commit(t, origin, "two")
	if err := m.Update(ctx, r); err != nil { // fetch
		t.Fatal(err)
	}
	if err := m.Bundle(ctx, testProject, "main", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(string(data), "=== call\n")[1:]
	if len(calls) != 3 {
		t.Fatalf("%d git calls, want 3 (clone, fetch, bundle):\n%s", len(calls), data)
	}
	_, knownHosts, _ := m.paths(testProject)
	sshRe := regexp.MustCompile(`^ssh -F /dev/null -i (/\S+/key-\d+/id) -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=30 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=` + regexp.QuoteMeta(knownHosts) + `$`)
	for i, call := range calls {
		name := []string{"clone", "fetch", "bundle"}[i]
		var args []string
		env := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(call), "\n") {
			switch {
			case strings.HasPrefix(line, "arg "):
				args = append(args, strings.TrimPrefix(line, "arg "))
			case strings.HasPrefix(line, "env "):
				k, v, _ := strings.Cut(strings.TrimPrefix(line, "env "), "=")
				env[k] = v
			}
		}
		if len(args) < 5 || strings.Join(args[:4], " ") != "-c core.hooksPath=/dev/null -c core.fsmonitor=false" {
			t.Errorf("%s: args %q do not start with the hook and fsmonitor options", name, args)
		}
		want := map[string]string{
			"PATH":                "/usr/bin:/bin",
			"HOME":                m.home,
			"GIT_ALLOW_PROTOCOL":  m.protocols,
			"GIT_TERMINAL_PROMPT": "0",
			"GIT_CONFIG_NOSYSTEM": "1",
			"GIT_CONFIG_GLOBAL":   "/dev/null",
		}
		for k, v := range want {
			if env[k] != v {
				t.Errorf("%s: %s=%q, want %q", name, k, env[k], v)
			}
		}
		// Nothing else is inherited; sh itself adds only these.
		for k := range env {
			switch k {
			case "PWD", "OLDPWD", "SHLVL", "_", "GIT_SSH_COMMAND":
			default:
				if _, ok := want[k]; !ok {
					t.Errorf("%s: unexpected variable %s", name, k)
				}
			}
		}
		if name == "bundle" {
			if env["GIT_SSH_COMMAND"] != "false" {
				t.Errorf("bundle: GIT_SSH_COMMAND=%q, want false", env["GIT_SSH_COMMAND"])
			}
			if got := strings.Join(args[4:], " "); got != "--git-dir="+filepath.Join(m.dir, testProject+".git")+" bundle create - refs/heads/main" {
				t.Errorf("bundle args %q", got)
			}
			continue
		}
		sm := sshRe.FindStringSubmatch(env["GIT_SSH_COMMAND"])
		if sm == nil {
			t.Errorf("%s: GIT_SSH_COMMAND=%q", name, env["GIT_SSH_COMMAND"])
		} else if _, err := os.Stat(sm[1]); !os.IsNotExist(err) {
			t.Errorf("%s: key file %s still exists after the command: %v", name, sm[1], err)
		}
		if !strings.Contains(call, "keymode 600\n") || !strings.Contains(call, "keydir 700\n") {
			t.Errorf("%s: key file or directory mode wrong:\n%s", name, call)
		}
		// "--" ends the options before the URL.
		dash := -1
		for j, a := range args {
			if a == "--" {
				dash = j
				break
			}
		}
		if dash < 0 || dash+1 >= len(args) || args[dash+1] != url {
			t.Errorf("%s: no -- before the URL in %q", name, args)
		}
	}
	if strings.Contains(string(data), "not a real key") {
		t.Error("the key reached the git command line or environment")
	}
}
