//go:build isolation

package isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/projectuid"
	"github.com/JCombee/wsm/internal/sandbox"
)

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// procStatus parses /proc/<pid>/status output.
func procStatus(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}

func TestProjectUserCannotLogin(t *testing.T) {
	ctx := ctxT(t)
	for _, p := range []project{s.a, s.b} {
		out, err := run(ctx, "getent", "passwd", p.user)
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Split(strings.TrimSpace(out), ":")
		if len(f) != 7 {
			t.Fatalf("passwd entry %q", out)
		}
		if f[2] != strconv.Itoa(p.uid) || f[5] != helper.HomeDir(p.uid) {
			t.Errorf("%s: uid %s home %s", p.user, f[2], f[5])
		}
		if f[6] != "/usr/sbin/nologin" {
			t.Errorf("%s: shell %q, want /usr/sbin/nologin", p.user, f[6])
		}
		out, err = run(ctx, "getent", "shadow", p.user)
		if err != nil {
			t.Fatal(err)
		}
		pw := strings.Split(out, ":")[1]
		if !strings.HasPrefix(pw, "!") && !strings.HasPrefix(pw, "*") {
			t.Errorf("%s: password field is not locked", p.user)
		}
		// A login shell for the user fails: nologin refuses.
		if out, err := run(ctx, "su", "-l", p.user, "-c", "id"); err == nil {
			t.Errorf("%s: su -l succeeded: %s", p.user, out)
		}
		// The home directory is private.
		fi, err := os.Stat(helper.HomeDir(p.uid))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: home mode %v, want 0700", p.user, fi.Mode().Perm())
		}
	}
}

func TestOtherProjectFilesAndSocket(t *testing.T) {
	ctx := ctxT(t)
	for _, tc := range []struct{ from, to project }{{s.b, s.a}, {s.a, s.b}} {
		sock := helper.PodmanSocket(tc.to.uid)
		// Control: the socket exists, and the manager's group can use it.
		fi, err := os.Stat(filepath.Dir(sock))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if fi.Mode().Perm() != 0o750 || fi.Mode()&os.ModeSetgid == 0 || int(st.Uid) != tc.to.uid {
			t.Errorf("%s: mode %v owner %d", filepath.Dir(sock), fi.Mode(), st.Uid)
		}
		if ok, out := probeAs(ctx, "wsm", "unix:"+sock); !ok {
			t.Errorf("control: wsm cannot reach %s: %s", sock, out)
		}
		for _, spec := range []string{
			"unix:" + sock,
			"readdir:" + helper.HomeDir(tc.to.uid),
			"readdir:" + filepath.Dir(sock),
			fmt.Sprintf("readdir:/run/user/%d", tc.to.uid),
			"readfile:" + filepath.Join(helper.HomeDir(tc.to.uid), ".config/systemd/user/podman.socket.d/wsm.conf"),
		} {
			if ok, out := probeAs(ctx, tc.from.user, spec); ok {
				t.Errorf("%s reached %s: %s", tc.from.user, spec, out)
			}
		}
	}
	// The project users are not in the manager's group.
	for _, p := range []project{s.a, s.b} {
		out, _ := run(ctx, "id", "-Gn", p.user)
		for _, g := range strings.Fields(out) {
			if g == "wsm" || g == "wsm-admin" {
				t.Errorf("%s is in group %s", p.user, g)
			}
		}
	}
}

type inspect struct {
	State struct {
		Pid int `json:"Pid"`
	} `json:"State"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	HostConfig struct {
		Privileged  bool     `json:"Privileged"`
		NetworkMode string   `json:"NetworkMode"`
		CapAdd      []string `json:"CapAdd"`
		SecurityOpt []string `json:"SecurityOpt"`
		Binds       []string `json:"Binds"`
		Devices     []any    `json:"Devices"`
	} `json:"HostConfig"`
	Config struct {
		User string `json:"User"`
	} `json:"Config"`
}

func inspectWork(t *testing.T, p project) inspect {
	t.Helper()
	out, err := asUser(ctxT(t), p, "podman", "container", "inspect", p.container)
	if err != nil {
		t.Fatal(err)
	}
	var ci []inspect
	if err := json.Unmarshal([]byte(out), &ci); err != nil || len(ci) != 1 {
		t.Fatalf("inspect: %v: %s", err, out)
	}
	return ci[0]
}

func TestContainerHardening(t *testing.T) {
	ctx := ctxT(t)
	p := s.a
	// pid 1 is the init process; self is the exec itself.
	for _, pid := range []string{"1", "self"} {
		out, err := inSandbox(ctx, p, "cat", "/proc/"+pid+"/status")
		if err != nil {
			t.Fatal(err)
		}
		st := procStatus(out)
		if st["NoNewPrivs"] != "1" {
			t.Errorf("pid %s: NoNewPrivs=%q", pid, st["NoNewPrivs"])
		}
		if st["Seccomp"] != "2" {
			t.Errorf("pid %s: Seccomp=%q, want 2 (filter)", pid, st["Seccomp"])
		}
		for _, k := range []string{"CapEff", "CapPrm", "CapInh", "CapAmb", "CapBnd"} {
			if n, err := strconv.ParseUint(st[k], 16, 64); err != nil || n != 0 {
				t.Errorf("pid %s: %s=%q, want 0", pid, k, st[k])
			}
		}
		for _, k := range []string{"Uid", "Gid"} {
			if f := strings.Fields(st[k]); len(f) != 4 || f[0] != "1000" || f[1] != "1000" || f[2] != "1000" || f[3] != "1000" {
				t.Errorf("pid %s: %s=%q, want all 1000", pid, k, st[k])
			}
		}
	}

	// Limits as the container sees them.
	want := map[string]string{
		"cpu.max":    testCPUQuota,
		"memory.max": strconv.Itoa(testMemory),
		"pids.max":   strconv.Itoa(testPids),
	}
	for f, w := range want {
		out, err := inSandbox(ctx, p, "cat", "/sys/fs/cgroup/"+f)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(strings.Fields(out), " "); got != w {
			t.Errorf("%s = %q, want %q", f, got, w)
		}
	}

	// uid 1000 in the container is the project user on the host.
	ci := inspectWork(t, p)
	if ci.State.Pid <= 0 {
		t.Fatal("container has no pid")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", ci.State.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if f := strings.Fields(procStatus(string(data))["Uid"]); len(f) != 4 || f[0] != strconv.Itoa(p.uid) {
		t.Errorf("host Uid of container pid 1 = %v, want %d", f, p.uid)
	}
	if ci.HostConfig.Privileged || ci.HostConfig.NetworkMode == "host" || len(ci.HostConfig.CapAdd) != 0 {
		t.Errorf("hostconfig privileged=%v network=%q capadd=%v", ci.HostConfig.Privileged,
			ci.HostConfig.NetworkMode, ci.HostConfig.CapAdd)
	}
	for _, o := range ci.HostConfig.SecurityOpt {
		if strings.Contains(o, "unconfined") {
			t.Errorf("security opt %q", o)
		}
	}
	if ci.Config.User != "1000:1000" {
		t.Errorf("user %q", ci.Config.User)
	}
	// No root to escalate to, and no writing outside the volumes.
	if out, err := inSandbox(ctx, p, "touch", "/etc/wsm-escape"); err == nil {
		t.Errorf("wrote /etc as the workspace user: %s", out)
	}
	if out, err := inSandbox(ctx, p, "touch", "/workspace/ok", "/home/agent/ok"); err != nil {
		t.Errorf("cannot write the own volumes: %s", out)
	}
}

func TestNoPodmanSocketInSandbox(t *testing.T) {
	ctx := ctxT(t)
	for _, p := range []project{s.a, s.b} {
		// Output and "Permission denied" errors come back mixed; only lines
		// that start with "/" are paths. The search runs as the workspace
		// user, so it sees what an agent can see.
		out, _ := inSandbox(ctx, p, "find", "/", "-xdev", "-type", "s")
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "/") {
				continue
			}
			// tmux keeps its own socket in /tmp/tmux-1000.
			if strings.HasPrefix(line, "/tmp/tmux-1000/") {
				continue
			}
			t.Errorf("%s: socket in the sandbox: %s", p.slug, line)
		}
		out, _ = inSandbox(ctx, p, "cat", "/proc/self/mountinfo")
		for _, bad := range []string{"podman.sock", "docker.sock", "/run/wsm", "/run/user/"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: mountinfo mentions %s", p.slug, bad)
			}
		}
		for _, path := range []string{
			helper.PodmanSocket(p.uid), fmt.Sprintf("/run/user/%d/podman/podman.sock", p.uid),
			"/run/podman/podman.sock", "/var/run/docker.sock", "/run/wsm/api.sock", "/run/wsm/helper.sock",
		} {
			if out, err := inSandbox(ctx, p, "test", "-e", path); err == nil {
				t.Errorf("%s: %s exists in the sandbox %s", p.slug, path, out)
			}
		}
		out, _ = inSandbox(ctx, p, "env")
		for _, v := range []string{"CONTAINER_HOST", "DOCKER_HOST", "XDG_RUNTIME_DIR"} {
			if strings.Contains(out, v+"=") {
				t.Errorf("%s: %s is set in the sandbox", p.slug, v)
			}
		}
		if !strings.Contains(out, "WSM_PROJECT="+p.slug) || !strings.Contains(out, "WSM_SANDBOX=main") {
			t.Errorf("%s: WSM_PROJECT/WSM_SANDBOX missing from env", p.slug)
		}
	}
}

func TestNoHostMounts(t *testing.T) {
	for _, p := range []project{s.a, s.b} {
		ci := inspectWork(t, p)
		want := map[string]string{
			sandbox.CodeVolume(p.sandboxID): sandbox.WorkspaceDir,
		}
		projects, err := s.api.Projects(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, pr := range projects {
			if pr.Slug == p.slug {
				want[sandbox.HomeVolume(pr.ID)] = sandbox.HomeDir
			}
		}
		if len(ci.Mounts) != 2 {
			t.Errorf("%s: %d mounts, want 2: %+v", p.slug, len(ci.Mounts), ci.Mounts)
		}
		for _, m := range ci.Mounts {
			if m.Type != "volume" || want[m.Name] != m.Destination {
				t.Errorf("%s: unexpected mount %+v", p.slug, m)
			}
			if !strings.HasPrefix(m.Source, helper.HomeDir(p.uid)+"/") {
				t.Errorf("%s: volume %s lives outside the project home: %s", p.slug, m.Name, m.Source)
			}
		}
		if len(ci.HostConfig.Binds) > 0 {
			for _, b := range ci.HostConfig.Binds {
				name, _, _ := strings.Cut(b, ":")
				if _, ok := want[name]; !ok {
					t.Errorf("%s: bind %q", p.slug, b)
				}
			}
		}
		if len(ci.HostConfig.Devices) != 0 {
			t.Errorf("%s: devices %v", p.slug, ci.HostConfig.Devices)
		}
	}
}

func TestLANAndMetadataBlocked(t *testing.T) {
	ctx := ctxT(t)
	p := s.a
	for _, tc := range []struct {
		host    string
		counter string
	}{
		{"10.0.0.1", "lan_blocked"},
		{"172.16.0.1", "lan_blocked"},
		{"192.168.0.1", "lan_blocked"},
		{"192.168.1.1", "lan_blocked"},
		{"100.64.0.1", "lan_blocked"},
		{"169.254.169.254", "linklocal_blocked"},
	} {
		blockedBy(t, tc.counter, "sandbox to "+tc.host, func() (bool, string) {
			return tcpFromSandbox(ctx, p, tc.host, 80)
		})
		blockedBy(t, tc.counter, "project user to "+tc.host, func() (bool, string) {
			return probeAs(ctx, p.user, "tcp:"+tc.host+":80")
		})
	}
	// Control: the internet stays open, so the blocks are not a dead network.
	if ok, out := tcpFromSandbox(ctx, p, "deb.debian.org", 80); !ok {
		t.Errorf("control: sandbox cannot reach deb.debian.org:80: %s", out)
	}
}

func TestHostServiceBlocked(t *testing.T) {
	ctx := ctxT(t)
	const port = 18082
	listener(t, fmt.Sprintf("0.0.0.0:%d", port))
	if s.hostAddr == "" {
		t.Fatal("no host address")
	}
	rootConnects(t, net.JoinHostPort(s.hostAddr, strconv.Itoa(port)))
	blockedBy(t, "host_blocked", "project user to the host address", func() (bool, string) {
		return probeAs(ctx, s.a.user, fmt.Sprintf("tcp:%s:%d", s.hostAddr, port))
	})
	// From the sandbox, pasta maps 169.254.1.2 to the host's address, and
	// host.containers.internal resolves to 169.254.1.2: host_blocked must
	// stop both.
	for _, h := range []string{"169.254.1.2", "host.containers.internal"} {
		blockedBy(t, "host_blocked", "sandbox to the host service via "+h, func() (bool, string) {
			return tcpFromSandbox(ctx, s.a, h, port)
		})
	}
	// pasta copies the host's address into the pod, so for the sandbox it is
	// its own address: the connection stays in the pod and meets no wsm rule.
	if ok, out := tcpFromSandbox(ctx, s.a, s.hostAddr, port); ok {
		t.Errorf("sandbox reached the host service via %s: %s", s.hostAddr, out)
	}
}

func TestWsm0OtherPortsBlocked(t *testing.T) {
	ctx := ctxT(t)
	const port = 9999
	listener(t, fmt.Sprintf("10.255.0.1:%d", port))
	rootConnects(t, fmt.Sprintf("10.255.0.1:%d", port))
	blockedBy(t, "wsm0_blocked", "sandbox to 10.255.0.1:9999", func() (bool, string) {
		return tcpFromSandbox(ctx, s.a, "10.255.0.1", port)
	})
	blockedBy(t, "wsm0_blocked", "project user to 10.255.0.1:9999", func() (bool, string) {
		return probeAs(ctx, s.b.user, fmt.Sprintf("tcp:10.255.0.1:%d", port))
	})
}

func TestManagerAPINotReachable(t *testing.T) {
	ctx := ctxT(t)
	// Control: the API answers root.
	if _, err := s.api.Projects(ctx); err != nil {
		t.Fatalf("control: %v", err)
	}
	for _, p := range []project{s.a, s.b} {
		if ok, out := probeAs(ctx, p.user, "unix:/run/wsm/api.sock"); ok {
			t.Errorf("%s reached the API socket: %s", p.user, out)
		}
		if out, err := inSandbox(ctx, p, "test", "-e", "/run/wsm"); err == nil {
			t.Errorf("%s: /run/wsm exists in the sandbox %s", p.slug, out)
		}
	}
	// The manager has no TCP listener in M1.
	out, err := run(ctx, "ss", "-Hltnp")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"wsm"`) {
			t.Errorf("wsm listens on TCP: %s", line)
		}
	}
}

func TestOtherProjectPreviewPort(t *testing.T) {
	ctx := ctxT(t)
	const port = 18080
	// The preview proxy comes in M5; until then the test publishes B's app
	// port itself, the way the plan describes, with the same hardening.
	name := "iso-preview"
	asUser(ctx, s.b, "podman", "rm", "-f", "--ignore", name)
	_, err := asUser(ctx, s.b, "podman", "run", "-d", "--name", name,
		"--user", "1000:1000", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--memory", "128m", "--pids-limit", "64", "--network", "pasta",
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", port),
		testImage, "nc", "-lk", "-p", "8080")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { asUser(context.Background(), s.b, "podman", "rm", "-f", "--ignore", name) })
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: root cannot reach B's preview: %v", err)
		}
		time.Sleep(time.Second)
	}
	blockedBy(t, "loopback_blocked", "project A user to B's preview", func() (bool, string) {
		return probeAs(ctx, s.a.user, fmt.Sprintf("tcp:127.0.0.1:%d", port))
	})
	for _, h := range []string{"169.254.1.2", "host.containers.internal"} {
		blockedBy(t, "host_blocked", "sandbox A to B's preview via "+h, func() (bool, string) {
			return tcpFromSandbox(ctx, s.a, h, port)
		})
	}
	// 127.0.0.1 and the host's address are pod A's own addresses (pasta
	// copies the latter into the pod), so these connections stay in pod A.
	for _, h := range []string{"127.0.0.1", s.hostAddr} {
		if ok, out := tcpFromSandbox(ctx, s.a, h, port); ok {
			t.Errorf("sandbox A reached B's preview via %s: %s", h, out)
		}
	}
}

func TestHelperRejectsOtherUser(t *testing.T) {
	ctx := ctxT(t)
	const victim = projectuid.Max
	// Project users cannot even open the socket.
	for _, p := range []project{s.a, s.b} {
		if ok, out := probeAs(ctx, p.user, "unix:"+helper.DefaultSocket); ok {
			t.Errorf("%s reached the helper socket: %s", p.user, out)
		}
	}
	// Root can open it, but is not the manager: the SO_PEERCRED check must
	// refuse a valid request.
	c := &helper.Client{Socket: helper.DefaultSocket}
	err := c.CreateProjectUser(ctx, victim)
	if err == nil || !strings.Contains(err.Error(), helper.ErrPeerRejected.Error()) {
		t.Errorf("root's request: %v, want %q", err, helper.ErrPeerRejected)
	}
	if _, err := user.Lookup(helper.UserName(victim)); err == nil {
		t.Errorf("user %s exists after a rejected request", helper.UserName(victim))
	}
	// Control: the helper serves the manager, which started with it.
	if out, err := run(ctx, "nft", "list", "table", "inet", "wsm"); err != nil {
		t.Errorf("control: base rules not loaded: %s", out)
	}
	if out, err := run(ctx, "ip", "-4", "addr", "show", "dev", "wsm0"); err != nil || !strings.Contains(out, "10.255.0.1/32") {
		t.Errorf("control: wsm0 not set up: %s", out)
	}
}

// TestSandboxLifecycle runs the rest of the CLI life cycle on the real
// installation: stop, start again, wsm shell and delete. Setup already ran
// create and start for the other sandboxes.
func TestSandboxLifecycle(t *testing.T) {
	ctx := ctxT(t)
	const slug = "life"
	target := s.a.slug + "/" + slug
	if _, err := wsm(ctx, "sandbox", "create", target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wsm(context.Background(), "sandbox", "delete", target) })
	sb, ok, err := findSandbox(ctx, s.a.slug, slug)
	if err != nil || !ok {
		t.Fatalf("sandbox %s not listed after create: %v", target, err)
	}
	life := s.a
	life.sandboxID, life.container = sb.ID, sandbox.WorkContainer(sb.ID)
	status := func() string {
		sb, _, err := findSandbox(ctx, s.a.slug, slug)
		if err != nil {
			t.Fatal(err)
		}
		return sb.Status
	}
	running := func() string {
		out, _ := asUser(ctx, life, "podman", "container", "inspect", "--format", "{{.State.Running}}", life.container)
		return strings.TrimSpace(out)
	}

	if _, err := wsm(ctx, "sandbox", "start", target); err != nil {
		t.Fatal(err)
	}
	if err := waitForExec(ctx, life); err != nil {
		t.Fatal(err)
	}

	if _, err := wsm(ctx, "sandbox", "stop", target); err != nil {
		t.Fatal(err)
	}
	if st := status(); st != "stopped" {
		t.Errorf("status after stop = %q, want stopped", st)
	}
	if r := running(); r != "false" {
		t.Errorf("workspace container after stop: running = %q, want false", r)
	}
	if out, err := wsm(ctx, "shell", target); err == nil {
		t.Errorf("wsm shell on a stopped sandbox succeeded: %s", out)
	}

	if _, err := wsm(ctx, "sandbox", "start", target); err != nil {
		t.Fatal(err)
	}
	if st, r := status(), running(); st != "running" || r != "true" {
		t.Errorf("after the second start: status %q, running %q", st, r)
	}
	if err := waitForExec(ctx, life); err != nil {
		t.Fatal(err)
	}
	shellRunsCommand(ctx, t, target)

	if _, err := wsm(ctx, "sandbox", "delete", target); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := findSandbox(ctx, s.a.slug, slug); err != nil || ok {
		t.Errorf("sandbox %s still listed after delete (err %v)", target, err)
	}
	if _, err := asUser(ctx, life, "podman", "pod", "exists", sandbox.PodName(sb.ID)); err == nil {
		t.Errorf("pod %s still exists", sandbox.PodName(sb.ID))
	}
	if _, err := asUser(ctx, life, "podman", "volume", "exists", sandbox.CodeVolume(sb.ID)); err == nil {
		t.Errorf("volume %s still exists", sandbox.CodeVolume(sb.ID))
	}
	// The project's sandboxes share its home volume, so it stays.
	if _, err := asUser(ctx, life, "podman", "volume", "exists", sandbox.HomeVolume(s.a.id)); err != nil {
		t.Errorf("home volume of %s gone after deleting one sandbox: %v", s.a.slug, err)
	}
}

func TestDeleteLeavesNothingBehind(t *testing.T) {
	ctx := ctxT(t)
	c, err := createProject(ctx, s, "iso-c")
	if err != nil {
		t.Fatal(err)
	}
	if err := shareImage(ctx, s, s.a, c); err != nil {
		t.Fatal(err)
	}
	if err := createSandbox(ctx, s, &c); err != nil {
		t.Fatal(err)
	}
	if _, err := inSandbox(ctx, c, "sh", "-c", "echo data > /workspace/f && echo data > /home/agent/f"); err != nil {
		t.Fatal(err)
	}
	if _, err := wsm(ctx, "project", "delete", c.slug); err != nil {
		t.Fatal(err)
	}

	if _, err := user.Lookup(c.user); err == nil {
		t.Errorf("user %s still exists", c.user)
	}
	if _, err := user.LookupGroup(c.user); err == nil {
		t.Errorf("group %s still exists", c.user)
	}
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		data, _ := os.ReadFile(f)
		if strings.Contains(string(data), c.user+":") {
			t.Errorf("%s still has %s", f, c.user)
		}
	}
	paths := []string{
		helper.HomeDir(c.uid),
		filepath.Dir(helper.PodmanSocket(c.uid)),
		fmt.Sprintf("/etc/tmpfiles.d/wsm-podman-%d.conf", c.uid),
		"/var/lib/systemd/linger/" + c.user,
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s still exists", p)
		}
	}
	// logind removes the runtime directory shortly after the last session.
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := os.Lstat(fmt.Sprintf("/run/user/%d", c.uid))
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("/run/user/%d still exists", c.uid)
			break
		}
		time.Sleep(time.Second)
	}
	out, _ := run(ctx, "pgrep", "-u", strconv.Itoa(c.uid))
	if strings.TrimSpace(out) != "" {
		t.Errorf("processes of uid %d still run: %s", c.uid, out)
	}
	out, _ = run(ctx, "find", "/home", "/var", "/tmp", "/run", "/etc", "-xdev", "-uid", strconv.Itoa(c.uid))
	if strings.TrimSpace(out) != "" {
		t.Errorf("files of uid %d left: %s", c.uid, out)
	}
	if sbs, err := s.api.Sandboxes(ctx, ""); err == nil {
		for _, sb := range sbs {
			if sb.Project == c.slug || sb.Project == "" {
				t.Errorf("sandbox row left: %+v", sb)
			}
		}
	}
	if ps, err := s.api.Projects(ctx); err == nil {
		for _, p := range ps {
			if p.Slug == c.slug {
				t.Errorf("project row left: %+v", p)
			}
		}
	}
}
