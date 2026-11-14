package gitmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultDir holds the mirrors, inside the manager's state directory.
const DefaultDir = "/var/lib/wsm/git"

// DefaultHome is HOME for git and ssh: the manager's state directory.
const DefaultHome = "/var/lib/wsm"

// DefaultGit is the git binary.
const DefaultGit = "/usr/bin/git"

// CommandTimeout bounds every git command on the host.
const CommandTimeout = 10 * time.Minute

// maxErrorTail caps the git and ssh output quoted in an error. It comes
// partly from the remote and is untrusted (ADR 0008, point 7).
const maxErrorTail = 4 << 10

// gitConfig disables hooks and fsmonitor for every git command, so nothing
// in a mirror can make git run a program (CLAUDE.md invariant 3).
var gitConfig = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// safePath matches the paths that go into GIT_SSH_COMMAND, which git runs
// through sh. Only paths wsm derives itself pass.
var safePath = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)

var idPattern = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

// Repo is what a git operation needs to know about a project.
type Repo struct {
	// ProjectID names the mirror; it must be a valid project ID.
	ProjectID string
	// URL is the validated SSH repository URL.
	URL string
	// Key is the private deploy key in OpenSSH format. It is written to a
	// 0600 file in a private directory for one git command at a time.
	Key []byte
}

// Mirrors keeps one bare mirror per project under Dir.
type Mirrors struct {
	dir  string
	home string
	git  string
	// protocols is GIT_ALLOW_PROTOCOL. Only tests allow more than ssh.
	protocols string
	// validURL checks a URL once more before git sees it.
	validURL func(string) error
}

// NewMirrors returns the mirrors in dir, which it creates with mode 0700.
// home is HOME for git and ssh. Key files left behind by a crash are
// removed.
func NewMirrors(dir, home, git string) (*Mirrors, error) {
	for _, p := range []string{dir, home, git} {
		if !safePath.MatchString(p) {
			return nil, fmt.Errorf("mirrors: unsafe path %q", p)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mirrors: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mirrors: %w", err)
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "key-*"))
	for _, d := range stale {
		if err := os.RemoveAll(d); err != nil {
			return nil, fmt.Errorf("mirrors: remove stale key: %w", err)
		}
	}
	return &Mirrors{dir: dir, home: home, git: git, protocols: "ssh", validURL: ValidRepoURL}, nil
}

func (m *Mirrors) paths(projectID string) (mirror, knownHosts string, err error) {
	if !idPattern.MatchString(projectID) {
		return "", "", fmt.Errorf("mirror: invalid project id %q", projectID)
	}
	return filepath.Join(m.dir, projectID+".git"), filepath.Join(m.dir, projectID+".known_hosts"), nil
}

// Path returns the mirror directory of a project.
func (m *Mirrors) Path(projectID string) (string, error) {
	p, _, err := m.paths(projectID)
	return p, err
}

// env returns the complete environment of a git command: nothing is
// inherited, and neither git nor ssh reads a system or user config.
func (m *Mirrors) env(sshCommand string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + m.home,
		"GIT_ALLOW_PROTOCOL=" + m.protocols,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_SSH_COMMAND=" + sshCommand,
	}
}

// sshCommand returns GIT_SSH_COMMAND for a key file and known_hosts file.
// Both paths come from wsm, never from the URL or other input.
func sshCommand(keyFile, knownHosts string) (string, error) {
	for _, p := range []string{keyFile, knownHosts} {
		if !safePath.MatchString(p) {
			return "", fmt.Errorf("unsafe path %q for ssh", p)
		}
	}
	return "ssh -F /dev/null -i " + keyFile +
		" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=30" +
		" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=" + knownHosts, nil
}

// withKey writes key to a 0600 file in a new 0700 directory, calls fn with
// its path and removes both afterwards.
func (m *Mirrors) withKey(key []byte, fn func(keyFile string) error) error {
	if len(key) == 0 {
		return errors.New("no deploy key")
	}
	dir, err := os.MkdirTemp(m.dir, "key-")
	if err != nil {
		return fmt.Errorf("key directory: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("key directory: %w", err)
	}
	keyFile := filepath.Join(dir, "id")
	f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("key file: %w", err)
	}
	_, werr := f.Write(key)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return fmt.Errorf("key file: %w", errors.Join(werr, cerr))
	}
	return fn(keyFile)
}

// run runs git with the fixed options and environment, without a shell.
// stdout goes to out when it is not nil. The error quotes at most the last
// 4 KiB of stderr, sanitized.
func (m *Mirrors) run(ctx context.Context, dir string, env []string, out io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.git, append(append([]string{}, gitConfig...), args...)...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = out
	errOut := &tail{max: maxErrorTail}
	cmd.Stderr = errOut
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return fmt.Errorf("git %s: %w: %s", subcommand(args), err, errOut)
	}
	return nil
}

// subcommand returns the first argument that is not an option.
func subcommand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return "command"
}

// Update brings a project's mirror up to date with its origin: a clone
// --mirror the first time, a fetch --prune afterwards.
func (m *Mirrors) Update(ctx context.Context, r Repo) error {
	mirror, knownHosts, err := m.paths(r.ProjectID)
	if err != nil {
		return err
	}
	if err := m.validURL(r.URL); err != nil {
		return err
	}
	return m.withKey(r.Key, func(keyFile string) error {
		ssh, err := sshCommand(keyFile, knownHosts)
		if err != nil {
			return err
		}
		env := m.env(ssh)
		if _, err := os.Stat(mirror); errors.Is(err, os.ErrNotExist) {
			return m.clone(ctx, env, r.URL, mirror)
		} else if err != nil {
			return fmt.Errorf("mirror: %w", err)
		}
		// The URL comes from the database, not from the mirror's config.
		return m.run(ctx, mirror, env, nil, "--git-dir="+mirror, "fetch", "--prune", "--quiet",
			"--", r.URL, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	})
}

// clone makes a new mirror. It clones next to the final path and renames,
// so a failed clone leaves no half mirror behind.
func (m *Mirrors) clone(ctx context.Context, env []string, url, mirror string) error {
	tmp := mirror + ".new"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	err := m.run(ctx, m.dir, env, nil, "clone", "--mirror", "--quiet", "--", url, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, mirror); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("mirror: %w", err)
	}
	return nil
}

// Bundle writes a git bundle of refs/heads/<branch> from a project's
// mirror to w.
func (m *Mirrors) Bundle(ctx context.Context, projectID, branch string, w io.Writer) error {
	mirror, _, err := m.paths(projectID)
	if err != nil {
		return err
	}
	if err := ValidBranch(branch); err != nil {
		return err
	}
	env := m.env("false")
	return m.run(ctx, mirror, env, w, "--git-dir="+mirror, "bundle", "create", "-", "refs/heads/"+branch)
}

// Remove deletes a project's mirror and its known_hosts file. Missing
// files are not an error.
func (m *Mirrors) Remove(projectID string) error {
	mirror, knownHosts, err := m.paths(projectID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(mirror + ".new"); err != nil {
		return fmt.Errorf("remove mirror: %w", err)
	}
	if err := os.RemoveAll(mirror); err != nil {
		return fmt.Errorf("remove mirror: %w", err)
	}
	if err := os.Remove(knownHosts); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove known_hosts: %w", err)
	}
	return nil
}

// tail keeps the last max bytes written to it.
type tail struct {
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string { return Sanitize(string(t.buf)) }

// Sanitize makes untrusted output safe to print on a terminal: invalid
// UTF-8 and control characters other than newline and tab become '?'.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		control := r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)
		if r == utf8.RuneError || (control && r != '\n' && r != '\t') {
			r = '?'
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
