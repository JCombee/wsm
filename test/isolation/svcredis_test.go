//go:build isolation

package isolation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/sandbox"
)

// The M3d tests use their own projects: iso-r (A) with two sandboxes and a
// redis ceiling of 8.10 and 8.8, and iso-q (B) with one sandbox and no
// redis ceiling. They talk to Redis with nc from the test image.

const (
	redisTag    = "8.10"
	redisTagAlt = "8.8"
	redisPort   = 6379
)

// redisFix holds the M3d projects.
type redisFix struct {
	a, a2, b project
	// volumes are iso-r's podman volumes before any redis ran.
	volumes []string
}

var (
	redisOnce sync.Once
	redisErr  error
	redisF    redisFix
)

// redisFixture creates iso-r and iso-q once.
func redisFixture(t *testing.T) redisFix {
	t.Helper()
	redisOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		redisErr = func() error {
			var err error
			if redisF.a, err = createProject(ctx, s, "iso-r"); err != nil {
				return err
			}
			if redisF.b, err = createProject(ctx, s, "iso-q"); err != nil {
				return err
			}
			if err := shareImage(ctx, s, s.a, redisF.a, redisF.b); err != nil {
				return err
			}
			if out, err := wsm(ctx, "project", "block", "set", "iso-r", "redis", "--tag", redisTag, "--tag", redisTagAlt); err != nil {
				return fmt.Errorf("block set iso-r: %v: %s", err, out)
			}
			if err := startNamedSandbox(ctx, &redisF.a, "main"); err != nil {
				return err
			}
			redisF.a2 = redisF.a
			if err := startNamedSandbox(ctx, &redisF.a2, "two"); err != nil {
				return err
			}
			if err := startNamedSandbox(ctx, &redisF.b, "main"); err != nil {
				return err
			}
			redisF.volumes, err = volumes(ctx, redisF.a)
			return err
		}()
	})
	if redisErr != nil {
		t.Fatalf("M3d fixture: %v", redisErr)
	}
	return redisF
}

// volumes lists the podman volumes of p's project user.
func volumes(ctx context.Context, p project) ([]string, error) {
	out, err := asUser(ctx, p, "podman", "volume", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	v := strings.Fields(out)
	slices.Sort(v)
	return v, nil
}

// redisContainer is the name of p's redis container.
func redisContainer(p project) string { return sandbox.ServiceContainer(p.sandboxID, "redis") }

// redisExists reports whether p's redis container exists.
func redisExists(ctx context.Context, p project) bool {
	_, err := asUser(ctx, p, "podman", "container", "exists", redisContainer(p))
	return err == nil
}

// redisID returns the ID of p's redis container.
func redisID(ctx context.Context, t *testing.T, p project) string {
	t.Helper()
	out, err := asUser(ctx, p, "podman", "container", "inspect", "--format", "{{.Id}}", redisContainer(p))
	if err != nil {
		t.Fatalf("inspect %s: %v", redisContainer(p), err)
	}
	return strings.TrimSpace(out)
}

// redisCmd sends inline Redis commands from p's workspace container to
// 127.0.0.1:6379 with nc and returns the raw reply.
func redisCmd(ctx context.Context, p project, cmds ...string) (string, error) {
	return inSandbox(ctx, p, "bash", "-c", `printf "$0" | nc -w 2 127.0.0.1 6379`,
		strings.Join(cmds, `\r\n`)+`\r\n`)
}

// redisVersion returns redis_version from INFO server, seen from p.
func redisVersion(ctx context.Context, t *testing.T, p project) string {
	t.Helper()
	out, err := redisCmd(ctx, p, "INFO server")
	if err != nil {
		t.Fatalf("INFO server from %s: %v", p.slug, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "redis_version:"); ok {
			return v
		}
	}
	t.Fatalf("INFO server from %s has no redis_version: %s", p.slug, lastBytes(out, 300))
	return ""
}

// sandboxServiceTag returns the tag in sandbox_services, or "" without row.
func sandboxServiceTag(ctx context.Context, t *testing.T, sandboxID, block string) string {
	t.Helper()
	sqlDB := openDB(ctx, t)
	defer sqlDB.Close()
	var tag string
	err := sqlDB.QueryRowContext(ctx, `SELECT tag FROM sandbox_services WHERE sandbox_id = ? AND block_id = ?`,
		sandboxID, block).Scan(&tag)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		t.Fatal(err)
	}
	return tag
}

// enableRedis calls service_enable for redis from p and checks the shape
// of the answer: block, tag, host and port, no account fields.
func enableRedis(ctx context.Context, t *testing.T, p project, args map[string]any) creds {
	t.Helper()
	r := mcpFromSandbox(ctx, t, p, mcpRequest{Op: "call", Tool: "service_enable", Arguments: args})
	if r.Error != "" || r.IsError {
		t.Fatalf("service_enable %v from %s: %s %s", args, p.slug, r.Error, r.Text)
	}
	var raw map[string]any
	if err := json.Unmarshal(r.Structured, &raw); err != nil {
		t.Fatalf("service_enable from %s: %v", p.slug, err)
	}
	for _, k := range []string{"user", "password", "database"} {
		if _, ok := raw[k]; ok {
			t.Errorf("service_enable for redis has %q", k)
		}
	}
	var c creds
	if err := json.Unmarshal(r.Structured, &c); err != nil {
		t.Fatal(err)
	}
	if c.Block != "redis" || c.Host != "127.0.0.1" || c.Port != redisPort {
		t.Errorf("service_enable for redis: %+v", c)
	}
	return c
}

// serviceDetails returns the result and detail of the service_enable
// events after seq.
func serviceDetails(ctx context.Context, t *testing.T, seq int64) [][2]string {
	t.Helper()
	sqlDB := openDB(ctx, t)
	defer sqlDB.Close()
	rows, err := sqlDB.QueryContext(ctx, `SELECT result, detail FROM audit_events
		WHERE tool = 'service_enable' AND seq > ? ORDER BY seq`, seq)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var r, d string
		if err := rows.Scan(&r, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, [2]string{r, d})
	}
	return out
}

// waitPong waits until redis answers PING from p.
func waitPong(ctx context.Context, t *testing.T, p project, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := redisCmd(ctx, p, "PING")
		if err == nil && out == "+PONG\r\n" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("redis in %s does not answer PING: %v %q", p.slug, err, lastBytes(out, 200))
		}
		time.Sleep(time.Second)
	}
}

func TestRedisUnreachableBeforeEnable(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	if out, err := wsm(ctx, "project", "block", "list", g.a.slug); err != nil || !strings.Contains(out, "redis") {
		t.Fatalf("precondition: iso-r has no redis ceiling: %v %s", err, out)
	}
	if ok, out := tcpFromSandbox(ctx, g.a, "127.0.0.1", redisPort); ok || !strings.Contains(out, "refused") {
		t.Errorf("iso-r/main to 127.0.0.1:6379 before service_enable: connected %v: %s", ok, out)
	}
	if redisExists(ctx, g.a) {
		t.Errorf("%s exists before service_enable", redisContainer(g.a))
	}
}

func TestRedisEnableGivesPrivateInstance(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	before := auditHead(ctx, t)
	c := enableRedis(ctx, t, g.a, map[string]any{"block": "redis"})
	if c.Tag != redisTag {
		t.Errorf("default tag %s, want %s", c.Tag, redisTag)
	}
	if ev := serviceDetails(ctx, t, before); len(ev) != 1 || ev[0] != [2]string{"ok", "granted redis " + redisTag} {
		t.Errorf("audit events %v, want one ok with detail granted redis %s", ev, redisTag)
	}

	if out, err := redisCmd(ctx, g.a, "PING"); err != nil || out != "+PONG\r\n" {
		t.Errorf("PING: %v %q", err, out)
	}
	if out, err := redisCmd(ctx, g.a, "SET wsmkey hello", "GET wsmkey"); err != nil || out != "+OK\r\n$5\r\nhello\r\n" {
		t.Errorf("SET/GET: %v %q", err, out)
	}
	if v := redisVersion(ctx, t, g.a); !strings.HasPrefix(v, redisTag+".") {
		t.Errorf("redis_version %s, want %s.x", v, redisTag)
	}

	id := redisID(ctx, t, g.a)
	if c2 := enableRedis(ctx, t, g.a, map[string]any{"block": "redis"}); c2 != c {
		t.Errorf("second service_enable: %+v, want %+v", c2, c)
	}
	if id2 := redisID(ctx, t, g.a); id2 != id {
		t.Errorf("the second call replaced the container: %s -> %s", id, id2)
	}

	for _, tool := range []string{"services_available", "sandbox_info"} {
		r := mcpFromSandbox(ctx, t, g.a, mcpRequest{Op: "call", Tool: tool})
		if r.Error != "" || r.IsError {
			t.Fatalf("%s: %s %s", tool, r.Error, r.Text)
		}
		switch tool {
		case "services_available":
			if !strings.Contains(r.Text, `"block":"redis"`) || !strings.Contains(r.Text, `"enabled_tags":["8.10"]`) {
				t.Errorf("services_available = %s", r.Text)
			}
		case "sandbox_info":
			if !strings.Contains(r.Text, `{"block":"redis","tag":"8.10","host":"127.0.0.1","port":6379}`) {
				t.Errorf("sandbox_info = %s", r.Text)
			}
		}
	}
}

// redisInspect is the part of podman container inspect the hardening test
// reads.
type redisInspect struct {
	State struct {
		Pid int `json:"Pid"`
	} `json:"State"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	HostConfig struct {
		ReadonlyRootfs bool            `json:"ReadonlyRootfs"`
		PortBindings   map[string]any  `json:"PortBindings"`
		Binds          []string        `json:"Binds"`
		Memory         int64           `json:"Memory"`
		PidsLimit      int64           `json:"PidsLimit"`
		CpuQuota       int64           `json:"CpuQuota"`
		Privileged     bool            `json:"Privileged"`
		CapAdd         []string        `json:"CapAdd"`
		Devices        json.RawMessage `json:"Devices"`
	} `json:"HostConfig"`
}

func TestRedisContainerHardened(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	enableRedis(ctx, t, g.a, map[string]any{"block": "redis"})
	name := redisContainer(g.a)

	out, err := asUser(ctx, g.a, "podman", "exec", name, "cat", "/proc/1/status")
	if err != nil {
		t.Fatal(err)
	}
	st := procStatus(out)
	if f := strings.Fields(st["Uid"]); len(f) != 4 || f[0] != "1000" || f[1] != "1000" {
		t.Errorf("Uid=%q, want 1000", st["Uid"])
	}
	for k, want := range map[string]string{"NoNewPrivs": "1", "Seccomp": "2"} {
		if st[k] != want {
			t.Errorf("%s=%q, want %s", k, st[k], want)
		}
	}
	for _, k := range []string{"CapEff", "CapBnd"} {
		if n, err := strconv.ParseUint(st[k], 16, 64); err != nil || n != 0 {
			t.Errorf("%s=%q, want 0", k, st[k])
		}
	}

	out, err = asUser(ctx, g.a, "podman", "container", "inspect", name)
	if err != nil {
		t.Fatal(err)
	}
	var ci []redisInspect
	if err := json.Unmarshal([]byte(out), &ci); err != nil || len(ci) != 1 {
		t.Fatalf("inspect: %v: %s", err, lastBytes(out, 500))
	}
	hc := ci[0].HostConfig
	if !hc.ReadonlyRootfs {
		t.Error("the root filesystem is writable")
	}
	if len(hc.PortBindings) != 0 {
		t.Errorf("port bindings: %v", hc.PortBindings)
	}
	if len(hc.Binds) != 0 {
		t.Errorf("binds: %v", hc.Binds)
	}
	for _, m := range ci[0].Mounts {
		if m.Type != "tmpfs" {
			t.Errorf("mount %+v, want only tmpfs", m)
		}
	}
	if hc.Memory != 268435456 || hc.PidsLimit != 64 || hc.CpuQuota != 50000 {
		t.Errorf("limits: memory %d pids %d cpu quota %d", hc.Memory, hc.PidsLimit, hc.CpuQuota)
	}
	if hc.Privileged || len(hc.CapAdd) != 0 {
		t.Errorf("privileged %v, cap add %v", hc.Privileged, hc.CapAdd)
	}

	// The pod shares one network namespace: read its sockets from the
	// workspace container. 0A is LISTEN, 18EB is 6379, 0100007F 127.0.0.1.
	listen := func(file string) []string {
		out, err := inSandbox(ctx, g.a, "cat", file)
		if err != nil {
			t.Fatal(err)
		}
		var l []string
		for _, line := range strings.Split(out, "\n")[1:] {
			f := strings.Fields(line)
			if len(f) > 3 && f[3] == "0A" && strings.HasSuffix(f[1], ":18EB") {
				l = append(l, f[1])
			}
		}
		return l
	}
	if l := listen("/proc/net/tcp"); !slices.Equal(l, []string{"0100007F:18EB"}) {
		t.Errorf("listeners on 6379: %v, want only 127.0.0.1", l)
	}
	if l := listen("/proc/net/tcp6"); len(l) != 0 {
		t.Errorf("tcp6 listeners on 6379: %v", l)
	}

	// On the host redis-server runs as the project user.
	if ci[0].State.Pid <= 0 {
		t.Fatal("the redis container has no pid")
	}
	comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", ci[0].State.Pid))
	if !strings.HasPrefix(string(comm), "redis-server") {
		t.Errorf("pid %d is %q, want redis-server", ci[0].State.Pid, comm)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", ci[0].State.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if f := strings.Fields(procStatus(string(data))["Uid"]); len(f) != 4 || f[0] != strconv.Itoa(g.a.uid) {
		t.Errorf("redis-server on the host runs as uid %v, want %d", f, g.a.uid)
	}

	if v, err := volumes(ctx, g.a); err != nil || !slices.Equal(v, g.volumes) {
		t.Errorf("volumes of iso-r: %v (%v), were %v", v, err, g.volumes)
	}
}

func TestRedisNotReachableFromOtherSandboxes(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	enableRedis(ctx, t, g.a, map[string]any{"block": "redis"})
	if out, err := redisCmd(ctx, g.a, "PING"); err != nil || out != "+PONG\r\n" {
		t.Fatalf("control: iso-r/main gets no PONG: %v %q", err, out)
	}
	hosts := []string{"127.0.0.1", "169.254.1.2"}
	if s.hostAddr != "" {
		hosts = append(hosts, s.hostAddr)
	} else {
		t.Error("no host address known")
	}
	for _, p := range []project{g.a2, g.b} {
		for _, h := range hosts {
			if ok, out := tcpFromSandbox(ctx, p, h, redisPort); ok {
				t.Errorf("%s/%s reached %s:6379: %s", p.slug, p.sandboxID, h, out)
			}
		}
	}
	c, err := net.DialTimeout("tcp", "127.0.0.1:6379", 5*time.Second)
	if err == nil {
		c.Close()
		t.Error("the host reached 127.0.0.1:6379")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("the host to 127.0.0.1:6379: %v, want connection refused", err)
	}
}

func TestRedisEnableOutsideCeilingDenied(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	before := auditHead(ctx, t)
	cases := []struct {
		name string
		p    project
		args map[string]any
	}{
		{"B without ceiling", g.b, map[string]any{"block": "redis"}},
		{"A with tag 8.4", g.a, map[string]any{"block": "redis", "tag": "8.4"}},
		{"A with a semicolon in the tag", g.a, map[string]any{"block": "redis", "tag": "8.10;id"}},
	}
	for _, tc := range cases {
		r := mcpFromSandbox(ctx, t, tc.p, mcpRequest{Op: "call", Tool: "service_enable", Arguments: tc.args})
		if r.Error != "" || !r.IsError || !strings.HasPrefix(r.Text, "denied:") {
			t.Errorf("%s: error %q isError %v text %q", tc.name, r.Error, r.IsError, r.Text)
		}
	}
	ev := serviceDetails(ctx, t, before)
	if len(ev) != len(cases) {
		t.Errorf("%d service_enable events, want %d: %v", len(ev), len(cases), ev)
	}
	for _, e := range ev {
		if e[0] != "denied" {
			t.Errorf("event %v, want denied", e)
		}
	}
	if redisExists(ctx, g.b) {
		t.Error("iso-q/main got a redis container")
	}
	if tag := sandboxServiceTag(ctx, t, g.b.sandboxID, "redis"); tag != "" {
		t.Errorf("iso-q/main has a sandbox_services row with tag %s", tag)
	}
}

func TestRedisTagSwitchReplaces(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	enableRedis(ctx, t, g.a, map[string]any{"block": "redis"})
	if out, err := redisCmd(ctx, g.a, "SET switchkey v1"); err != nil || out != "+OK\r\n" {
		t.Fatalf("SET: %v %q", err, out)
	}
	id := redisID(ctx, t, g.a)

	c := enableRedis(ctx, t, g.a, map[string]any{"block": "redis", "tag": redisTagAlt})
	if c.Tag != redisTagAlt {
		t.Errorf("tag %s, want %s", c.Tag, redisTagAlt)
	}
	if v := redisVersion(ctx, t, g.a); !strings.HasPrefix(v, redisTagAlt+".") {
		t.Errorf("redis_version %s, want %s.x", v, redisTagAlt)
	}
	if id2 := redisID(ctx, t, g.a); id2 == id {
		t.Error("the container was not replaced")
	}
	if out, err := redisCmd(ctx, g.a, "GET switchkey"); err != nil || out != "$-1\r\n" {
		t.Errorf("GET after the switch: %v %q, want nil", err, out)
	}
	r := mcpFromSandbox(ctx, t, g.a, mcpRequest{Op: "call", Tool: "sandbox_info"})
	if !strings.Contains(r.Text, `{"block":"redis","tag":"8.8",`) {
		t.Errorf("sandbox_info = %s", r.Text)
	}
	if tag := sandboxServiceTag(ctx, t, g.a.sandboxID, "redis"); tag != redisTagAlt {
		t.Errorf("sandbox_services tag %s, want %s", tag, redisTagAlt)
	}

	enableRedis(ctx, t, g.a, map[string]any{"block": "redis", "tag": redisTag})
	if v := redisVersion(ctx, t, g.a); !strings.HasPrefix(v, redisTag+".") {
		t.Errorf("back to %s: redis_version %s", redisTag, v)
	}
}

func TestRedisAfterStopStart(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	enableRedis(ctx, t, g.a, map[string]any{"block": "redis"})
	if out, err := redisCmd(ctx, g.a, "SET stopkey v1"); err != nil || out != "+OK\r\n" {
		t.Fatalf("SET: %v %q", err, out)
	}
	target := g.a.slug + "/main"
	for _, op := range []string{"stop", "start"} {
		if out, err := wsm(ctx, "sandbox", op, target); err != nil {
			t.Fatalf("%s %s: %v: %s", op, target, err, out)
		}
	}
	if err := waitForExec(ctx, g.a); err != nil {
		t.Fatal(err)
	}
	waitPong(ctx, t, g.a, 60*time.Second)
	if out, err := redisCmd(ctx, g.a, "GET stopkey"); err != nil || out != "$-1\r\n" {
		t.Errorf("GET after stop/start: %v %q, want nil", err, out)
	}
}

func TestRedisCeilingRules(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	out, err := wsm(ctx, "project", "block", "set", g.b.slug, "redis", "--tag", "99.99")
	if err == nil || !strings.Contains(out, "99.99") {
		t.Errorf("block set iso-q redis 99.99: %v: %s; want a failure about the tag", err, out)
	}
	if out, err := wsm(ctx, "project", "block", "list", g.b.slug); err != nil || strings.Contains(out, "redis") {
		t.Errorf("iso-q has a redis ceiling after the failure: %v: %s", err, out)
	}

	enableRedis(ctx, t, g.a2, map[string]any{"block": "redis"})
	before, err := wsm(ctx, "project", "block", "list", g.a.slug)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"project", "block", "set", g.a.slug, "redis", "--tag", redisTagAlt},
		{"project", "block", "remove", g.a.slug, "redis"},
	} {
		out, err := wsm(ctx, args...)
		if err == nil || !strings.Contains(out, "two") {
			t.Errorf("wsm %v: %v: %s; want a failure naming two", args, err, out)
		}
	}
	for _, req := range []struct{ method, body string }{
		{http.MethodPut, `{"tags":["8.8"]}`},
		{http.MethodDelete, ""},
	} {
		code, body := apiStatus(ctx, t, req.method, "/v1/projects/"+g.a.slug+"/blocks/redis", req.body)
		if code != http.StatusConflict || !strings.Contains(body, "two") {
			t.Errorf("%s: %d %s, want 409 naming two", req.method, code, body)
		}
	}
	if after, err := wsm(ctx, "project", "block", "list", g.a.slug); err != nil || after != before {
		t.Errorf("ceiling changed:\n%s\nwas:\n%s", after, before)
	}
}

func TestSandboxDeleteRemovesRedis(t *testing.T) {
	ctx := ctxService(t)
	g := redisFixture(t)
	enableRedis(ctx, t, g.a, map[string]any{"block": "redis"})
	if !redisExists(ctx, g.a) {
		t.Fatal("precondition: no redis container in iso-r/main")
	}
	if out, err := wsm(ctx, "sandbox", "delete", g.a.slug+"/main"); err != nil {
		t.Fatalf("delete iso-r/main: %v: %s", err, out)
	}
	if redisExists(ctx, g.a) {
		t.Errorf("%s still exists", redisContainer(g.a))
	}
	if tag := sandboxServiceTag(ctx, t, g.a.sandboxID, "redis"); tag != "" {
		t.Errorf("sandbox_services row left with tag %s", tag)
	}
	v, err := volumes(ctx, g.a)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range v {
		if strings.Contains(name, g.a.sandboxID) {
			t.Errorf("volume %s of the deleted sandbox is left", name)
		}
	}
}
