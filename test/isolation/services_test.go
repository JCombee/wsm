//go:build isolation

package isolation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/api"
	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/secret"
)

// The shared service tests use two MariaDB versions side by side.
const (
	tagOld = "11.8"
	tagNew = "12.3"
	wsm0IP = "10.255.0.1"
)

// ctxService gives tests that may pull images more time than ctxT.
func ctxService(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// helperProbe runs as user wsm: it sends one raw request from stdin to the
// helper socket, unchanged, and prints the raw response.
func helperProbe(stdin io.Reader, stdout io.Writer) int {
	req, err := io.ReadAll(io.LimitReader(stdin, helper.MaxMessage+1))
	if err != nil || len(req) > helper.MaxMessage {
		fmt.Fprintf(stdout, "probe helper: request too large or unreadable: %v\n", err)
		return 1
	}
	conn, err := net.DialTimeout("unix", helper.DefaultSocket, 5*time.Second)
	if err != nil {
		fmt.Fprintf(stdout, "probe helper: %v\n", err)
		return 1
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Minute))
	conn.Write(req)
	conn.(*net.UnixConn).CloseWrite()
	resp, err := io.ReadAll(io.LimitReader(conn, helper.MaxMessage+1))
	if err != nil {
		fmt.Fprintf(stdout, "probe helper: read: %v\n", err)
		return 1
	}
	stdout.Write(resp)
	return 0
}

// helperAsWsm sends a raw request to the helper as user wsm, the only peer
// the helper serves, and returns the decoded response.
func helperAsWsm(ctx context.Context, t *testing.T, req string) helper.Response {
	t.Helper()
	cmd := exec.CommandContext(ctx, "runuser", "-u", "wsm", "--", s.probeBin)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", probeEnv + "=helper"}
	cmd.Stdin = strings.NewReader(req)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper probe as wsm: %v: %s", err, out)
	}
	var resp helper.Response
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("helper response %q: %v", out, err)
	}
	return resp
}

// svcUser returns the UID of wsm-svc.
func svcUser(t *testing.T) int {
	t.Helper()
	u, err := user.Lookup(helper.ServiceUser)
	if err != nil {
		t.Fatalf("look up %s: %v", helper.ServiceUser, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	return uid
}

// asSvcEnv runs a command as wsm-svc with extra environment variables, so
// a secret can reach a command without being in its argv.
func asSvcEnv(ctx context.Context, t *testing.T, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	uid := svcUser(t)
	cmd := exec.CommandContext(ctx, "runuser", append([]string{"-u", helper.ServiceUser, "--"}, args...)...)
	cmd.Dir = "/"
	cmd.Env = append([]string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + helper.ServiceHome,
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid),
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func asSvc(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	return asSvcEnv(ctx, t, nil, args...)
}

// services returns the instances through the API, by tag.
func services(ctx context.Context, t *testing.T) map[string]api.Service {
	t.Helper()
	svcs, err := s.api.Services(ctx)
	if err != nil {
		t.Fatalf("services: %v", err)
	}
	out := map[string]api.Service{}
	for _, v := range svcs {
		out[v.Tag] = v
	}
	return out
}

// instancePort returns the port of the instance for tag, failing the test
// when there is none.
func instancePort(ctx context.Context, t *testing.T, tag string) int {
	t.Helper()
	v, ok := services(ctx, t)[tag]
	if !ok {
		t.Fatalf("no instance for mariadb %s", tag)
	}
	return v.Port
}

// greeting reads the MariaDB greeting on addr and returns the version.
func greeting(addr string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	n, err := io.ReadAtLeast(c, buf, 6)
	if err != nil {
		return "", err
	}
	buf = buf[:n]
	if buf[4] != 0x0a {
		return "", fmt.Errorf("protocol %#x", buf[4])
	}
	end := bytes.IndexByte(buf[5:], 0)
	if end < 0 {
		return "", errors.New("no version")
	}
	return strings.TrimPrefix(string(buf[5:5+end]), "5.5.5-"), nil
}

// waitGreeting waits until addr answers with a version for tag.
func waitGreeting(t *testing.T, addr, tag string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v, err := greeting(addr)
		if err == nil && strings.HasPrefix(v, tag+".") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no greeting for %s within %s: %q, %v", addr, tag, timeout, v, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// svcElements returns the uid/port pairs in svc_allow4.
func svcElements(ctx context.Context, t *testing.T) [][2]int {
	t.Helper()
	out, err := run(ctx, "nft", "-j", "list", "set", "inet", "wsm", "svc_allow4")
	if err != nil {
		t.Fatalf("list svc_allow4: %v: %s", err, out)
	}
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("parse svc_allow4: %v", err)
	}
	var pairs [][2]int
	for _, obj := range doc.Nftables {
		raw, ok := obj["set"]
		if !ok {
			continue
		}
		var set struct {
			Elem []struct {
				Concat []any `json:"concat"`
			} `json:"elem"`
		}
		if err := json.Unmarshal(raw, &set); err != nil {
			t.Fatalf("parse svc_allow4 set: %v", err)
		}
		for _, e := range set.Elem {
			if len(e.Concat) != 3 {
				t.Fatalf("element %v", e.Concat)
			}
			uid, _ := e.Concat[0].(float64)
			addr, _ := e.Concat[1].(string)
			port, _ := e.Concat[2].(float64)
			if addr != wsm0IP {
				t.Errorf("element with address %q", addr)
			}
			pairs = append(pairs, [2]int{int(uid), int(port)})
		}
	}
	return pairs
}

func countElements(pairs [][2]int, uid, port int) int {
	n := 0
	for _, p := range pairs {
		if p[0] == uid && (port == 0 || p[1] == port) {
			n++
		}
	}
	return n
}

func grantReq(typ string, uid, port int) string {
	return fmt.Sprintf(`{"type":%q,"uid":%d,"port":%d}`, typ, uid, port)
}

// greetingFromSandbox reads the first bytes the service on port sends to
// p's workspace container.
func greetingFromSandbox(ctx context.Context, p project, port int) (bool, string) {
	out, err := inSandbox(ctx, p, "timeout", "5", "bash", "-c",
		`exec 3<>"/dev/tcp/$0/$1" && head -c 40 <&3`, wsm0IP, strconv.Itoa(port))
	return err == nil, out
}

func TestServiceInstancesPerVersion(t *testing.T) {
	ctx := ctxService(t)
	if out, err := wsm(ctx, "project", "block", "set", s.a.slug, "mariadb", "--tag", tagOld, "--tag", tagNew); err != nil {
		t.Fatalf("block set: %v: %s", err, out)
	}
	out, err := wsm(ctx, "project", "block", "list", s.a.slug)
	if err != nil || !regexp.MustCompile(`(?m)^mariadb\s+dev\s+11\.8,12\.3\s*$`).MatchString(out) {
		t.Fatalf("block list: %v: %s", err, out)
	}
	out, err = wsm(ctx, "service", "list")
	if err != nil {
		t.Fatalf("service list: %v: %s", err, out)
	}
	ports := map[string]int{}
	for _, tag := range []string{tagOld, tagNew} {
		re := regexp.MustCompile(`(?m)^mariadb\s+` + regexp.QuoteMeta(tag) + `\s+10\.255\.0\.1:(\d+)\s+ready\s+` +
			regexp.QuoteMeta(tag) + `\.\S+\s*$`)
		m := re.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("service list has no ready %s:\n%s", tag, out)
		}
		port, _ := strconv.Atoi(m[1])
		if port < 20000 || port > 20999 {
			t.Errorf("%s on port %d", tag, port)
		}
		ports[tag] = port
		v, err := greeting(fmt.Sprintf("%s:%d", wsm0IP, port))
		if err != nil || !strings.HasPrefix(v, tag+".") {
			t.Errorf("root reads %q, %v on the port of %s", v, err, tag)
		}
	}
	if ports[tagOld] == ports[tagNew] {
		t.Fatalf("both versions on port %d", ports[tagOld])
	}

	// Regression from preflight 2: after a restart of the helper, the user
	// manager of wsm-svc must still be able to (re)start an instance.
	if out, err := run(ctx, "systemctl", "restart", "wsm-helper.service"); err != nil {
		t.Fatalf("restart wsm-helper: %v: %s", err, out)
	}
	if out, err := asSvc(ctx, t, "systemctl", "--user", "restart", "wsm-mariadb-11-8.service"); err != nil {
		t.Fatalf("restart the 11.8 unit: %v: %s", err, out)
	}
	waitGreeting(t, fmt.Sprintf("%s:%d", wsm0IP, ports[tagOld]), tagOld, 2*time.Minute)

	// Another project shares the 11.8 instance.
	if out, err := wsm(ctx, "project", "block", "set", s.b.slug, "mariadb", "--tag", tagOld); err != nil {
		t.Fatalf("block set for B: %v: %s", err, out)
	}
	svcs := services(ctx, t)
	if len(svcs) != 2 || svcs[tagOld].Port != ports[tagOld] {
		t.Errorf("after B: %+v", svcs)
	}
	// Setting the same ceiling again changes nothing.
	if out, err := wsm(ctx, "project", "block", "set", s.a.slug, "mariadb", "--tag", tagOld, "--tag", tagNew); err != nil {
		t.Fatalf("second block set: %v: %s", err, out)
	}
	svcs = services(ctx, t)
	if len(svcs) != 2 || svcs[tagOld].Port != ports[tagOld] || svcs[tagNew].Port != ports[tagNew] {
		t.Errorf("after the second set: %+v", svcs)
	}
	if out, err := wsm(ctx, "project", "block", "list", s.a.slug); err != nil ||
		!regexp.MustCompile(`(?m)^mariadb\s+dev\s+11\.8,12\.3\s*$`).MatchString(out) {
		t.Errorf("block list after the second set: %v: %s", err, out)
	}
}

func TestBlockCeilingRejectsBadInput(t *testing.T) {
	ctx := ctxService(t)
	before, err := wsm(ctx, "project", "block", "list", s.a.slug)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"redis", "--tag", tagOld},
		{"mariadb", "--tag", tagOld, "--profile", "prod"},
		{"mariadb", "--tag", "latest"},
		{"mariadb", "--tag", "11"},
		{"mariadb", "--tag", "10.6"},
		{"mariadb", "--tag", "11.8;id"},
		{"mariadb", "--tag", "99.9"},
	} {
		full := append([]string{"project", "block", "set", s.a.slug}, args...)
		if out, err := wsm(ctx, full...); err == nil {
			t.Errorf("wsm %v succeeded: %s", full, out)
		}
	}
	// A tag that does not exist is invalid input (M3c).
	if code, body := apiStatus(ctx, t, "PUT", "/v1/projects/"+s.a.slug+"/blocks/mariadb", `{"tags":["99.9"]}`); code != 400 ||
		!strings.Contains(body, "does not exist") {
		t.Errorf("unknown tag through the API: %d %s, want 400", code, body)
	}
	if after, err := wsm(ctx, "project", "block", "list", s.a.slug); err != nil || after != before {
		t.Errorf("ceiling changed:\n%s\nwas:\n%s", after, before)
	}
	sqlDB := openDB(ctx, t)
	defer sqlDB.Close()
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM service_instances WHERE tag = '99.9'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows for 99.9: %d, %v", n, err)
	}
	unit := filepath.Join(helper.ServiceUnitDir(svcUser(t)), "wsm-mariadb-99-9.container")
	if _, err := os.Lstat(unit); err == nil {
		t.Errorf("%s exists", unit)
	}
}

func TestServiceRunsAsWsmSvc(t *testing.T) {
	ctx := ctxT(t)
	svc := svcUser(t)
	out, err := run(ctx, "ss", "-Hltnpe")
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{tagOld, tagNew} {
		port := instancePort(ctx, t, tag)
		var lines []string
		for _, l := range strings.Split(out, "\n") {
			f := strings.Fields(l)
			if len(f) >= 4 && strings.HasSuffix(f[3], ":"+strconv.Itoa(port)) {
				lines = append(lines, l)
			}
		}
		if len(lines) != 1 {
			t.Errorf("port %d has %d listeners: %q", port, len(lines), lines)
			continue
		}
		l := lines[0]
		if f := strings.Fields(l); f[3] != fmt.Sprintf("%s:%d", wsm0IP, port) {
			t.Errorf("port %d listens on %s", port, f[3])
		}
		if !strings.Contains(l, fmt.Sprintf("uid:%d ", svc)) {
			t.Errorf("port %d is not owned by wsm-svc: %s", port, l)
		}
		if !strings.Contains(l, fmt.Sprintf("cgroup:/user.slice/user-%d.slice/user@%d.service/", svc, svc)) {
			t.Errorf("port %d is not in wsm-svc's user manager: %s", port, l)
		}
		for _, h := range []string{"127.0.0.1", s.hostAddr} {
			if c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", h, port), 3*time.Second); err == nil {
				c.Close()
				t.Errorf("root reached %s:%d", h, port)
			}
		}
	}

	names, err := asSvc(ctx, t, "podman", "ps", "--format", "{{.Names}}")
	if err != nil {
		t.Fatalf("podman ps as wsm-svc: %v: %s", err, names)
	}
	for _, c := range []string{"wsm-mariadb-11-8", "wsm-mariadb-12-3"} {
		if !regexp.MustCompile(`(?m)^` + c + `$`).MatchString(names) {
			t.Errorf("wsm-svc does not run %s: %s", c, names)
			continue
		}
		limits, err := asSvc(ctx, t, "podman", "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.PidsLimit}}", c)
		if err != nil || strings.TrimSpace(limits) != "1073741824 512" {
			t.Errorf("limits of %s: %q, %v", c, limits, err)
		}
	}
	for _, p := range []project{s.a, s.b} {
		out, err := asUser(ctx, p, "podman", "ps", "-a", "--format", "{{.Names}}")
		if err != nil {
			t.Fatalf("podman ps as %s: %v", p.user, err)
		}
		if strings.Contains(out, "wsm-mariadb") {
			t.Errorf("%s sees the service containers: %s", p.user, out)
		}
		if ok, out := probeAs(ctx, p.user, "readdir:"+helper.ServiceHome); ok {
			t.Errorf("%s reads %s: %s", p.user, helper.ServiceHome, out)
		}
	}
}

func TestServiceBlockedWithoutGrant(t *testing.T) {
	ctx := ctxT(t)
	if out, err := wsm(ctx, "project", "block", "list", s.a.slug); err != nil || !strings.Contains(out, "mariadb") {
		t.Fatalf("precondition: A has no mariadb ceiling: %s", out)
	}
	if n := countElements(svcElements(ctx, t), s.a.uid, 0); n != 0 {
		t.Fatalf("precondition: A has %d grants", n)
	}
	for _, tag := range []string{tagOld, tagNew} {
		port := instancePort(ctx, t, tag)
		blockedBy(t, "wsm0_blocked", "sandbox A to "+tag, func() (bool, string) {
			return tcpFromSandbox(ctx, s.a, wsm0IP, port)
		})
		blockedBy(t, "wsm0_blocked", "project user A to "+tag, func() (bool, string) {
			return probeAs(ctx, s.a.user, fmt.Sprintf("tcp:%s:%d", wsm0IP, port))
		})
	}
}

func TestServiceGrantAndRevoke(t *testing.T) {
	ctx := ctxT(t)
	portOld, portNew := instancePort(ctx, t, tagOld), instancePort(ctx, t, tagNew)
	if r := helperAsWsm(ctx, t, grantReq("allow_service", s.a.uid, portOld)); !r.OK {
		t.Fatalf("allow_service: %s", r.Error)
	}
	t.Cleanup(func() { helperAsWsm(context.Background(), t, grantReq("revoke_service", s.a.uid, portOld)) })

	before := counter(t, "svc_allowed")
	ok, out := greetingFromSandbox(ctx, s.a, portOld)
	if !ok || !strings.Contains(out, tagOld+".") {
		t.Errorf("sandbox A with a grant: %v %q", ok, out)
	}
	if after := counter(t, "svc_allowed"); after <= before {
		t.Errorf("svc_allowed did not move (%d -> %d)", before, after)
	}
	blockedBy(t, "wsm0_blocked", "sandbox A to "+tagNew, func() (bool, string) {
		return tcpFromSandbox(ctx, s.a, wsm0IP, portNew)
	})
	blockedBy(t, "wsm0_blocked", "sandbox B to "+tagOld, func() (bool, string) {
		return tcpFromSandbox(ctx, s.b, wsm0IP, portOld)
	})

	if r := helperAsWsm(ctx, t, grantReq("allow_service", s.a.uid, portOld)); !r.OK {
		t.Fatalf("second allow_service: %s", r.Error)
	}
	if n := countElements(svcElements(ctx, t), s.a.uid, portOld); n != 1 {
		t.Errorf("%d elements after two allows, want 1", n)
	}

	if r := helperAsWsm(ctx, t, grantReq("revoke_service", s.a.uid, portOld)); !r.OK {
		t.Fatalf("revoke_service: %s", r.Error)
	}
	if n := countElements(svcElements(ctx, t), s.a.uid, 0); n != 0 {
		t.Errorf("%d elements of A after revoke", n)
	}
	blockedBy(t, "wsm0_blocked", "sandbox A after revoke", func() (bool, string) {
		return tcpFromSandbox(ctx, s.a, wsm0IP, portOld)
	})
	if r := helperAsWsm(ctx, t, grantReq("revoke_service", s.a.uid, portOld)); !r.OK {
		t.Errorf("second revoke_service: %s", r.Error)
	}
}

func TestServiceGrantsGoWithProjectUser(t *testing.T) {
	ctx := ctxT(t)
	port := instancePort(ctx, t, tagOld)
	p, err := createProject(ctx, s, "iso-s")
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []int{p.uid, s.a.uid} {
		if r := helperAsWsm(ctx, t, grantReq("allow_service", uid, port)); !r.OK {
			t.Fatalf("allow_service for %d: %s", uid, r.Error)
		}
	}
	t.Cleanup(func() { helperAsWsm(context.Background(), t, grantReq("revoke_service", s.a.uid, port)) })
	if n := countElements(svcElements(ctx, t), p.uid, port); n != 1 {
		t.Fatalf("control: iso-s has %d elements", n)
	}
	if out, err := wsm(ctx, "project", "delete", p.slug); err != nil {
		t.Fatalf("delete iso-s: %v: %s", err, out)
	}
	els := svcElements(ctx, t)
	if n := countElements(els, p.uid, 0); n != 0 {
		t.Errorf("%d elements of uid %d left after deleting its project", n, p.uid)
	}
	if n := countElements(els, s.a.uid, port); n != 1 {
		t.Errorf("A's element is gone: %d", n)
	}
	if r := helperAsWsm(ctx, t, grantReq("revoke_service", s.a.uid, port)); !r.OK {
		t.Errorf("revoke A: %s", r.Error)
	}
}

// serviceState is what an invalid helper request must leave unchanged.
func serviceState(ctx context.Context, t *testing.T) string {
	t.Helper()
	var b strings.Builder
	dir := helper.ServiceUnitDir(svcUser(t))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "unit %s %s\n", e.Name(), hex.EncodeToString(sum[:]))
	}
	els := svcElements(ctx, t)
	sort.Slice(els, func(i, j int) bool { return fmt.Sprint(els[i]) < fmt.Sprint(els[j]) })
	fmt.Fprintf(&b, "elements %v\n", els)
	// "secret ls" shows UpdatedAt relative to now; inspect gives the time.
	names, err := asSvc(ctx, t, "podman", "secret", "ls", "--noheading", "--format", "{{.Name}}")
	if err != nil {
		t.Fatalf("podman secret ls: %v: %s", err, names)
	}
	list := strings.Fields(names)
	sort.Strings(list)
	for _, n := range list {
		info, err := asSvc(ctx, t, "podman", "secret", "inspect", "--format", "{{.ID}} {{.UpdatedAt}}", n)
		if err != nil {
			t.Fatalf("podman secret inspect %s: %v: %s", n, err, info)
		}
		fmt.Fprintf(&b, "secret %s %s\n", n, strings.TrimSpace(info))
	}
	return b.String()
}

func TestHelperRejectsServiceRequests(t *testing.T) {
	ctx := ctxT(t)
	before := serviceState(ctx, t)
	pw := strings.Repeat("A", helper.PasswordLen)
	a := s.a.uid
	bad := []string{
		// ports
		grantReq("allow_service", a, 19999),
		grantReq("allow_service", a, 21000),
		grantReq("allow_service", a, 7443),
		grantReq("revoke_service", a, 22),
		`{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":7443,"password":"` + pw + `"}`,
		// uids
		grantReq("allow_service", 0, 20000),
		grantReq("allow_service", 49999, 20000),
		grantReq("allow_service", 60000, 20000),
		fmt.Sprintf(`{"type":"allow_service","uid":%d,"port":20000}`, svcUser(t)),
		`{"type":"allow_service","uid":"50000","port":20000}`,
		// services
		`{"type":"pull_service_image","service":"redis","tag":"7.2"}`,
		`{"type":"pull_service_image","service":"MariaDB","tag":"11.8"}`,
		`{"type":"pull_service_image","service":"../mariadb","tag":"11.8"}`,
		// tags
		`{"type":"pull_service_image","service":"mariadb","tag":"latest"}`,
		`{"type":"pull_service_image","service":"mariadb","tag":"10.6"}`,
		`{"type":"pull_service_image","service":"mariadb","tag":"11.8;id"}`,
		`{"type":"pull_service_image","service":"mariadb","tag":"11.8 --tls-verify=false"}`,
		`{"type":"ensure_service_instance","service":"mariadb","tag":"../11.8","port":20000,"password":"` + pw + `"}`,
		// passwords
		`{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw[:42] + `"}`,
		`{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw + `A"}`,
		`{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw[:42] + `+"}`,
		`{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw[:42] + `\n"}`,
		// missing fields
		`{"type":"pull_service_image","service":"mariadb"}`,
		`{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000}`,
		`{"type":"allow_service","uid":50000}`,
		`{"type":"revoke_service","port":20000}`,
		// extra fields
		`{"type":"pull_service_image","service":"mariadb","tag":"11.8","image":"docker.io/library/busybox:latest"}`,
		`{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw + `","unit":"[Service]\nExecStart=/bin/sh"}`,
		`{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw + `","path":"/etc/passwd"}`,
		`{"type":"allow_service","uid":50000,"port":20000,"command":"nft flush ruleset"}`,
		`{"type":"allow_service","uid":50000,"port":20000,"address":"0.0.0.0/0"}`,
		`{"type":"allow_service","uid":50000,"port":20000,"service":"mariadb"}`,
		`{"type":"pull_service_image","service":"mariadb","tag":"11.8","password":""}`,
		`{"type":"setup_wsm0","port":20000}`,
		// other spellings
		`{"type":"allow_service","UID":50000,"port":20000}`,
		`{"type":"allow_service","uid":50000,"Port":20000}`,
		`{"type":"Allow_Service","uid":50000,"port":20000}`,
		`{"type":"pull_service_image","Service":"mariadb","tag":"11.8"}`,
		`{"type":"pull_service_image","service":"mariadb","tag":"11.8","tag":"latest"}`,
	}
	for _, req := range bad {
		if r := helperAsWsm(ctx, t, req); r.OK {
			t.Errorf("helper accepted %s", req)
		} else if strings.Contains(r.Error, pw[:20]) {
			t.Errorf("helper error repeats the password: %s", r.Error)
		}
	}
	if after := serviceState(ctx, t); after != before {
		t.Errorf("state changed:\n%s\nwas:\n%s", after, before)
	}
	for _, p := range []project{s.a, s.b} {
		if ok, out := probeAs(ctx, p.user, "unix:"+helper.DefaultSocket); ok {
			t.Errorf("%s reached the helper socket: %s", p.user, out)
		}
	}
}

// instancePassword opens the root password of the instance for tag with the
// manager's key, the way the manager does.
func instancePassword(ctx context.Context, t *testing.T, tag string) string {
	t.Helper()
	key, err := os.ReadFile(secret.DefaultKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openDB(ctx, t)
	defer sqlDB.Close()
	var id string
	var sealed []byte
	if err := sqlDB.QueryRowContext(ctx, `SELECT id, secret_enc FROM service_instances
		WHERE block_id = 'mariadb' AND tag = ?`, tag).Scan(&id, &sealed); err != nil {
		t.Fatal(err)
	}
	pw, err := box.Open(sealed, []byte(id))
	if err != nil {
		t.Fatalf("open the password of %s: %v", tag, err)
	}
	if !helper.ValidPassword(string(pw)) {
		t.Fatalf("the password of %s has the wrong form", tag)
	}
	return string(pw)
}

// rootLogin logs in as root over TCP inside the container, with the
// password only in the environment of podman and the client.
func rootLogin(ctx context.Context, t *testing.T, container, pw string) (bool, string) {
	t.Helper()
	out, err := asSvcEnv(ctx, t, []string{"MYSQL_PWD=" + pw},
		"podman", "exec", "-e", "MYSQL_PWD", container,
		"mariadb", "-h127.0.0.1", "--protocol=tcp", "-uroot", "-N", "-e", "SELECT 42")
	// The client may print a TLS warning before the result.
	return err == nil && regexp.MustCompile(`(?m)^42$`).MatchString(out), out
}

func TestServiceRootPasswordNotLeaked(t *testing.T) {
	ctx := ctxT(t)
	pw := instancePassword(ctx, t, tagOld)
	const container = "wsm-mariadb-11-8"
	if ok, out := rootLogin(ctx, t, container, pw); !ok {
		t.Fatalf("control: root login with the stored password fails: %s", out)
	}
	wrong := pw[:42] + "x"
	if pw[42] == 'x' {
		wrong = pw[:42] + "y"
	}
	if ok, _ := rootLogin(ctx, t, container, wrong); ok {
		t.Fatal("root login with a wrong password works")
	}

	found := func(where string, data []byte) {
		if bytes.Contains(data, []byte(pw)) {
			t.Errorf("the root password is in %s", where)
		}
	}
	for _, root := range []string{"/etc/containers/systemd", "/var/lib/wsm"} {
		n := 0
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				found(path, data)
				n++
			}
			return nil
		})
		if err != nil || n == 0 {
			t.Errorf("walk %s: %d files, %v", root, n, err)
		}
	}
	if _, err := os.Stat("/var/lib/wsm/wsm.db-wal"); err != nil {
		t.Logf("no wsm.db-wal: %v", err)
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		if data, err := os.ReadFile(p); err == nil {
			found(p, data)
		}
	}
	cmd := exec.CommandContext(ctx, "journalctl", "--no-pager", "-o", "cat")
	cmd.Dir = "/"
	journal, err := cmd.Output()
	if err != nil || len(journal) == 0 {
		t.Errorf("journalctl: %v", err)
	}
	found("the journal", journal)
	for _, c := range []string{"wsm-mariadb-11-8", "wsm-mariadb-12-3"} {
		out, err := asSvc(ctx, t, "podman", "inspect", "--format", "{{json .Config.Env}}", c)
		if err != nil {
			t.Errorf("inspect %s: %v: %s", c, err, out)
		}
		found("Config.Env of "+c, []byte(out))
		if strings.Contains(out, "MARIADB_ROOT_PASSWORD") {
			t.Errorf("Config.Env of %s names the root password: %s", c, out)
		}
	}
	for _, args := range [][]string{{"service", "list"}, {"project", "block", "list", s.a.slug}} {
		out, err := wsm(ctx, args...)
		if err != nil {
			t.Errorf("wsm %v: %v", args, err)
		}
		found(fmt.Sprintf("wsm %v", args), []byte(out))
	}
	if fi, err := os.Stat(helper.ServiceHome); err != nil {
		t.Errorf("stat %s: %v", helper.ServiceHome, err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("%s has mode %v, want 0700", helper.ServiceHome, fi.Mode().Perm())
	}
}
