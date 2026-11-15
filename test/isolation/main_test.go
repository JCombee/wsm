//go:build isolation

package isolation

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/api"
	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/sandbox"
)

// marker exists only on a disposable VM (spikes/lib.sh, Makefile).
const marker = "/etc/wsm-spike-disposable"

// probeEnv makes the test binary run one read-only probe and exit. The
// tests use it to act as another user: "runuser -u USER -- binary".
const probeEnv = "WSM_ISOLATION_PROBE"

// The test image, built by project A from testdata/Containerfile.
const testImage = "localhost/wsm-test:latest"

//go:embed testdata/Containerfile
var containerfile []byte

// Limits of the test projects; TestContainerHardening checks them.
const (
	testCPUs     = "1"
	testCPUQuota = "100000 100000"
	testMemory   = 512 << 20
	testPids     = 256
)

const wsmBin = "/usr/local/bin/wsm"

// testSlugs are all projects the tests create; setup and teardown delete
// them.
var testSlugs = []string{"iso-a", "iso-b", "iso-c", "iso-d", "iso-e", "iso-f", "iso-g", "iso-h", "iso-s"}

// project is a project created for the tests, with one running sandbox.
type project struct {
	slug, id, user string
	uid            int
	sandboxID      string
	container      string
}

// suite is the state shared by the tests, set up once by TestMain.
type suite struct {
	a, b     project
	probeBin string // a copy of this binary that every user can run
	hostAddr string // the host's address towards the internet
	tmp      string
	api      *api.Client
}

var s *suite

func TestMain(m *testing.M) {
	if p := os.Getenv(probeEnv); p != "" {
		os.Exit(probe(p))
	}
	if _, err := os.Stat(marker); err != nil {
		fmt.Fprintf(os.Stderr, "refusing to run: %s is missing; run isolation tests only on a disposable VM\n", marker)
		os.Exit(3)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "refusing to run: isolation tests must run as root")
		os.Exit(4)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	var err error
	s, err = setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolation setup failed: %v\n", err)
		teardown()
		cancel()
		os.Exit(1)
	}
	code := m.Run()
	teardown()
	cancel()
	os.Exit(code)
}

// probe runs as an arbitrary user and only reads or connects. It prints
// what happened and exits 0 when the action succeeded.
func probe(spec string) int {
	kind, target, _ := strings.Cut(spec, ":")
	if kind == "mcp" {
		// Runs inside a workspace container; see mcp_test.go.
		return mcpProbe(os.Stdin, os.Stdout)
	}
	if kind == "helper" {
		// Runs as user wsm; see services_test.go.
		return helperProbe(os.Stdin, os.Stdout)
	}
	var err error
	switch kind {
	case "unix", "tcp":
		var c net.Conn
		c, err = net.DialTimeout(kind, target, 5*time.Second)
		if err == nil {
			c.Close()
		}
	case "readdir":
		_, err = os.ReadDir(target)
	case "readfile":
		_, err = os.ReadFile(target)
	default:
		err = fmt.Errorf("unknown probe %q", kind)
	}
	if err != nil {
		fmt.Printf("probe %s: %v\n", spec, err)
		return 1
	}
	fmt.Printf("probe %s: ok\n", spec)
	return 0
}

// run executes a command and returns its combined output.
func run(ctx context.Context, name string, args ...string) (string, error) {
	return runIn(ctx, nil, name, args...)
}

func runIn(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// asUser runs a command as a project user, with the environment rootless
// Podman needs.
func asUser(ctx context.Context, p project, args ...string) (string, error) {
	return asUserIn(ctx, p, nil, args...)
}

// asUserIn is asUser with stdin.
func asUserIn(ctx context.Context, p project, stdin io.Reader, args ...string) (string, error) {
	full := append([]string{"-u", p.user, "--", "env",
		"HOME=" + helper.HomeDir(p.uid),
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", p.uid),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", p.uid),
	}, args...)
	return runIn(ctx, stdin, "runuser", full...)
}

// inSandbox runs a command in a project's workspace container.
func inSandbox(ctx context.Context, p project, args ...string) (string, error) {
	return asUser(ctx, p, append([]string{"podman", "exec", p.container}, args...)...)
}

// wsm runs the wsm CLI as root, which can open the API socket.
func wsm(ctx context.Context, args ...string) (string, error) {
	return run(ctx, wsmBin, args...)
}

func setup(ctx context.Context) (*suite, error) {
	st := &suite{api: api.NewClient(api.DefaultSocket)}
	if out, err := run(ctx, "systemctl", "is-active", "wsm.service", "wsm-helper.socket"); err != nil {
		return nil, fmt.Errorf("wsm is not running; run deploy/install.sh first: %s", out)
	}

	// A copy of this binary in a directory every user can traverse.
	tmp, err := os.MkdirTemp("", "wsm-isolation-")
	if err != nil {
		return nil, err
	}
	st.tmp = tmp
	if err := os.Chmod(tmp, 0o755); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return nil, err
	}
	st.probeBin = filepath.Join(tmp, "probe")
	if err := os.WriteFile(st.probeBin, data, 0o755); err != nil {
		return nil, err
	}

	out, err := run(ctx, "ip", "-4", "route", "get", "1.1.1.1")
	if err != nil {
		return nil, err
	}
	if m := regexp.MustCompile(`\bsrc (\S+)`).FindStringSubmatch(out); m != nil {
		st.hostAddr = m[1]
	}

	// Leftovers of an earlier run.
	for _, slug := range testSlugs {
		wsm(ctx, "project", "delete", slug)
	}

	if st.a, err = createProject(ctx, st, "iso-a"); err != nil {
		return st, err
	}
	if st.b, err = createProject(ctx, st, "iso-b"); err != nil {
		return st, err
	}
	if err := buildImage(ctx, st, st.a); err != nil {
		return st, err
	}
	if err := shareImage(ctx, st, st.a, st.b); err != nil {
		return st, err
	}
	for _, p := range []*project{&st.a, &st.b} {
		if err := createSandbox(ctx, st, p); err != nil {
			return st, err
		}
	}
	return st, nil
}

func teardown() {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, slug := range testSlugs {
		wsm(ctx, "project", "delete", slug)
	}
	removeOrigin(ctx)
	if s.tmp != "" {
		os.RemoveAll(s.tmp)
	}
}

func createProject(ctx context.Context, st *suite, slug string) (project, error) {
	return createProjectFrom(ctx, st, slug, "--image", testImage)
}

// createProjectFrom creates a project with the test limits and the image
// source in source: "--image REF" or "--containerfile PATH".
func createProjectFrom(ctx context.Context, st *suite, slug string, source ...string) (project, error) {
	args := append([]string{"project", "create", "--slug", slug}, source...)
	args = append(args, "--cpus", testCPUs, "--memory", strconv.Itoa(testMemory), "--pids", strconv.Itoa(testPids))
	_, err := wsm(ctx, args...)
	if err != nil {
		return project{}, err
	}
	ps, err := st.api.Projects(ctx)
	if err != nil {
		return project{}, err
	}
	for _, p := range ps {
		if p.Slug == slug {
			return project{slug: slug, id: p.ID, user: p.LinuxUser, uid: p.LinuxUID}, nil
		}
	}
	return project{}, fmt.Errorf("project %s not listed after create", slug)
}

// buildImage builds the test image as the project user, from a context
// directory that user can read.
func buildImage(ctx context.Context, st *suite, p project) error {
	dir := filepath.Join(st.tmp, "build")
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), containerfile, 0o644); err != nil {
		return err
	}
	_, err := asUser(ctx, p, "podman", "build", "--quiet", "--tag", testImage, dir)
	return err
}

// shareImage copies the test image from one project user to another.
func shareImage(ctx context.Context, st *suite, from project, to ...project) error {
	archive := filepath.Join(st.tmp, "image-"+from.slug+".tar")
	if _, err := os.Stat(archive); err != nil {
		f, err := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		f.Close()
		if err := os.Chown(archive, from.uid, from.uid); err != nil {
			return err
		}
		if _, err := asUser(ctx, from, "podman", "save", "--quiet", "--output", archive, testImage); err != nil {
			return err
		}
		if err := os.Chmod(archive, 0o644); err != nil {
			return err
		}
	}
	for _, p := range to {
		if _, err := asUser(ctx, p, "podman", "load", "--quiet", "--input", archive); err != nil {
			return err
		}
	}
	return nil
}

func createSandbox(ctx context.Context, st *suite, p *project) error {
	target := p.slug + "/main"
	if _, err := wsm(ctx, "sandbox", "create", target); err != nil {
		return err
	}
	if _, err := wsm(ctx, "sandbox", "start", target); err != nil {
		return err
	}
	sbs, err := st.api.Sandboxes(ctx, p.slug)
	if err != nil {
		return err
	}
	if len(sbs) != 1 || sbs[0].Status != "running" {
		return fmt.Errorf("sandboxes of %s: %+v", p.slug, sbs)
	}
	p.sandboxID = sbs[0].ID
	p.container = sandbox.WorkContainer(p.sandboxID)
	return waitForExec(ctx, *p)
}

// waitForExec waits until exec works in p's workspace container.
func waitForExec(ctx context.Context, p project) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := inSandbox(ctx, p, "true")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Second)
	}
}

// findSandbox looks a sandbox up through the API.
func findSandbox(ctx context.Context, projectSlug, slug string) (api.Sandbox, bool, error) {
	sbs, err := s.api.Sandboxes(ctx, projectSlug)
	if err != nil {
		return api.Sandbox{}, false, err
	}
	for _, sb := range sbs {
		if sb.Slug == slug {
			return sb, true, nil
		}
	}
	return api.Sandbox{}, false, nil
}

// lockedBuffer is a bytes.Buffer that a command writes to while the test
// reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lastBytes returns at most the last n bytes of s, for error messages.
func lastBytes(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// shellRunsCommand runs wsm shell on target with pipes instead of a
// terminal, runs one command in the sandbox's tmux session and leaves the
// shell, after which wsm shell must end on its own.
func shellRunsCommand(ctx context.Context, t *testing.T, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, wsmBin, "shell", target)
	cmd.Dir = "/"
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Only running the command prints the marker: the echoed input shows
	// $((6*7)), not 42. Input that arrives before tmux is ready can get
	// lost, so the command repeats until the marker shows.
	const marker = "wsm-shell-42"
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(out.String(), marker) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("wsm shell %s: no %q in the output: %q", target, marker, lastBytes(out.String(), 2000))
		}
		io.WriteString(stdin, "echo wsm-shell-$((6*7))\r")
		time.Sleep(2 * time.Second)
	}
	// Leaving the shell ends the tmux session, the exec and wsm shell.
	io.WriteString(stdin, "exit\r")
	if err := cmd.Wait(); err != nil {
		t.Errorf("wsm shell %s did not end cleanly: %v: %q", target, err, lastBytes(out.String(), 2000))
	}
}

// probeAs runs a probe as user and reports whether it succeeded.
func probeAs(ctx context.Context, user, spec string) (bool, string) {
	cmd := exec.CommandContext(ctx, "runuser", "-u", user, "--", s.probeBin)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", probeEnv + "=" + spec}
	out, err := cmd.CombinedOutput()
	return err == nil, strings.TrimSpace(string(out))
}

// counter returns the packet count of a named counter in table inet wsm.
func counter(t *testing.T, name string) int64 {
	t.Helper()
	out, err := run(context.Background(), "nft", "list", "counter", "inet", "wsm", name)
	if err != nil {
		t.Fatalf("read counter %s: %v", name, err)
	}
	m := regexp.MustCompile(`packets (\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no packets in counter %s: %s", name, out)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// tcpFromSandbox connects from p's workspace container to host:port and
// reports whether the connection succeeded.
func tcpFromSandbox(ctx context.Context, p project, host string, port int) (bool, string) {
	out, err := inSandbox(ctx, p, "timeout", "5", "bash", "-c", `exec 3<>"/dev/tcp/$0/$1"`, host, strconv.Itoa(port))
	return err == nil, strings.TrimSpace(out)
}

// blockedBy checks that fn fails to connect and that the named counter of
// the wsm rules moved, so the wsm rule did the blocking.
func blockedBy(t *testing.T, counterName, what string, fn func() (bool, string)) {
	t.Helper()
	before := counter(t, counterName)
	ok, out := fn()
	after := counter(t, counterName)
	if ok {
		t.Errorf("%s: connected, want blocked", what)
		return
	}
	if after <= before {
		t.Errorf("%s: failed but counter %s did not move (%d -> %d): %s", what, counterName, before, after, out)
	}
}

// listener accepts and closes TCP connections on addr until the test ends.
func listener(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			c.Close()
		}
	}()
}

// rootConnects checks the control: root itself reaches addr.
func rootConnects(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("control: root cannot reach %s: %v", addr, err)
	}
	c.Close()
}
