//go:build isolation

package isolation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/JCombee/wsm/internal/api"
	"github.com/JCombee/wsm/internal/sandbox"
	"github.com/JCombee/wsm/internal/secret"
)

// The M3c tests use their own projects: iso-m (A) with two sandboxes and
// iso-n (B) with one. s.a keeps no ceiling of its own here, because
// TestMCPServiceEnableDenied relies on that.

// sqlRequest tells the SQL probe what to run. It arrives on stdin, so the
// password is never in argv.
type sqlRequest struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	User       string   `json:"user"`
	Password   string   `json:"password"`
	Database   string   `json:"database"`
	Statements []string `json:"statements"`
}

type sqlResult struct {
	OK    bool    `json:"ok"`
	Errno int     `json:"errno"`
	Rows  [][]any `json:"rows"`
}

// sqlResponse is what the SQL probe prints. It never holds the password.
type sqlResponse struct {
	ConnectError string      `json:"connect_error"`
	ConnectErrno int         `json:"connect_errno"`
	Results      []sqlResult `json:"results"`
}

// sqlProbe runs inside a workspace container: it connects with the
// driver, without TLS and without multi-statements, runs each statement
// on one session and prints one JSON line.
func sqlProbe(stdin io.Reader, stdout io.Writer) int {
	var req sqlRequest
	var resp sqlResponse
	defer func() { json.NewEncoder(stdout).Encode(resp) }()
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&req); err != nil {
		resp.ConnectError = "decode request: " + err.Error()
		return 1
	}
	scrub := func(s string) string {
		if req.Password == "" {
			return s
		}
		return strings.ReplaceAll(s, req.Password, "[password]")
	}
	cfg := mysql.NewConfig()
	cfg.User = req.User
	cfg.Passwd = req.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	cfg.DBName = req.Database
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		resp.ConnectError = scrub(err.Error())
		return 1
	}
	sqlDB := sql.OpenDB(connector)
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := sqlDB.Conn(ctx)
	if err == nil {
		err = conn.PingContext(ctx)
	}
	if err != nil {
		resp.ConnectError = scrub(err.Error())
		var me *mysql.MySQLError
		if errors.As(err, &me) {
			resp.ConnectErrno = int(me.Number)
		}
		return 1
	}
	defer conn.Close()
	resp.Results = []sqlResult{}
	for _, q := range req.Statements {
		r := sqlResult{Rows: [][]any{}}
		rows, err := conn.QueryContext(ctx, q)
		if err == nil {
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err = rows.Scan(ptrs...); err != nil {
					break
				}
				for i, v := range vals {
					if b, ok := v.([]byte); ok {
						vals[i] = string(b)
					}
				}
				r.Rows = append(r.Rows, vals)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
		if err != nil {
			var me *mysql.MySQLError
			if errors.As(err, &me) {
				r.Errno = int(me.Number)
			} else {
				r.Errno = -1
			}
		} else {
			r.OK = true
		}
		resp.Results = append(resp.Results, r)
	}
	return 0
}

// creds are the connection details from service_enable.
type creds struct {
	Block    string `json:"block"`
	Tag      string `json:"tag"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
}

// seenPasswords collects every grant password the tests saw, for the leak
// check in TestServiceEnableAudited.
var seenPasswords sync.Map

// sqlFromSandbox runs the SQL probe in p's workspace container. The test
// output never shows the password.
func sqlFromSandbox(ctx context.Context, t *testing.T, p project, c creds, stmts ...string) sqlResponse {
	t.Helper()
	installProbe(ctx, t, p)
	in, _ := json.Marshal(sqlRequest{Host: c.Host, Port: c.Port, User: c.User, Password: c.Password,
		Database: c.Database, Statements: stmts})
	out, err := asUserIn(ctx, p, bytes.NewReader(in), "podman", "exec", "-i",
		"-e", probeEnv+"=sql", p.container, probeInSandbox)
	if c.Password != "" {
		out = strings.ReplaceAll(out, c.Password, "[password]")
	}
	var resp sqlResponse
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if jerr := json.Unmarshal([]byte(lines[len(lines)-1]), &resp); jerr != nil {
		t.Fatalf("sql probe in %s: %v (%v): %s", p.slug, jerr, err, lastBytes(out, 500))
	}
	return resp
}

// enable calls service_enable from p and returns the credentials. A
// failure shows only the error text, which never holds a password.
func enable(ctx context.Context, t *testing.T, p project, args map[string]any) creds {
	t.Helper()
	r := mcpFromSandbox(ctx, t, p, mcpRequest{Op: "call", Tool: "service_enable", Arguments: args})
	if r.Error != "" || r.IsError {
		t.Fatalf("service_enable %v from %s: %s %s", args, p.slug, r.Error, r.Text)
	}
	var c creds
	if err := json.Unmarshal(r.Structured, &c); err != nil || c.Password == "" {
		t.Fatalf("service_enable from %s: no credentials in the result (%v)", p.slug, err)
	}
	var text creds
	if err := json.Unmarshal([]byte(r.Text), &text); err != nil || text != c {
		t.Errorf("service_enable from %s: text and structured content differ", p.slug)
	}
	seenPasswords.Store(c.Password, true)
	return c
}

// grantFix holds the M3c projects.
type grantFix struct {
	a, a2, b project
}

var (
	grantOnce sync.Once
	grantErr  error
	grantF    grantFix
)

// grantFixture creates iso-m and iso-n once: A with a mariadb ceiling of
// 11.8 and 12.3 and two sandboxes, B without a ceiling and one sandbox.
func grantFixture(t *testing.T) grantFix {
	t.Helper()
	grantOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		grantErr = func() error {
			var err error
			if grantF.a, err = createProject(ctx, s, "iso-m"); err != nil {
				return err
			}
			if grantF.b, err = createProject(ctx, s, "iso-n"); err != nil {
				return err
			}
			if err := shareImage(ctx, s, s.a, grantF.a, grantF.b); err != nil {
				return err
			}
			if out, err := wsm(ctx, "project", "block", "set", "iso-m", "mariadb", "--tag", tagOld, "--tag", tagNew); err != nil {
				return fmt.Errorf("block set iso-m: %v: %s", err, out)
			}
			if err := startNamedSandbox(ctx, &grantF.a, "main"); err != nil {
				return err
			}
			grantF.a2 = grantF.a
			if err := startNamedSandbox(ctx, &grantF.a2, "two"); err != nil {
				return err
			}
			return startNamedSandbox(ctx, &grantF.b, "main")
		}()
	})
	if grantErr != nil {
		t.Fatalf("M3c fixture: %v", grantErr)
	}
	return grantF
}

// startNamedSandbox creates and starts sandbox slug in p and points p at it.
func startNamedSandbox(ctx context.Context, p *project, slug string) error {
	target := p.slug + "/" + slug
	if out, err := wsm(ctx, "sandbox", "create", target); err != nil {
		return fmt.Errorf("create %s: %v: %s", target, err, out)
	}
	if out, err := wsm(ctx, "sandbox", "start", target); err != nil {
		return fmt.Errorf("start %s: %v: %s", target, err, out)
	}
	sb, ok, err := findSandbox(ctx, p.slug, slug)
	if err != nil || !ok || sb.Status != "running" {
		return fmt.Errorf("sandbox %s: %+v %v %v", target, sb, ok, err)
	}
	p.sandboxID = sb.ID
	p.container = sandbox.WorkContainer(sb.ID)
	return waitForExec(ctx, *p)
}

// apiStatus sends one request to the manager API and returns the status
// and body.
func apiStatus(ctx context.Context, t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", api.DefaultSocket)
		},
	}, Timeout: 15 * time.Minute}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://wsm"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(data)
}

// rootSQL runs one query as root on the instance for tag, from the host,
// and returns the first column of each row.
func rootSQL(ctx context.Context, t *testing.T, tag, query string, args ...any) []string {
	t.Helper()
	cfg := mysql.NewConfig()
	cfg.User = "root"
	cfg.Passwd = instancePassword(ctx, t, tag)
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", wsm0IP, instancePort(ctx, t, tag))
	cfg.Timeout = 5 * time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := sql.OpenDB(connector)
	defer sqlDB.Close()
	rows, err := sqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("root on %s: %s: %v", tag, query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// grantRows returns the number of grant rows, for one sandbox or all.
func grantRows(ctx context.Context, t *testing.T, sandboxID string) int {
	t.Helper()
	sqlDB := openDB(ctx, t)
	defer sqlDB.Close()
	q, args := `SELECT count(*) FROM service_grants`, []any{}
	if sandboxID != "" {
		q, args = q+` WHERE sandbox_id = ?`, append(args, sandboxID)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sortedPairs(ctx context.Context, t *testing.T) [][2]int {
	els := svcElements(ctx, t)
	sort.Slice(els, func(i, j int) bool {
		if els[i][0] != els[j][0] {
			return els[i][0] < els[j][0]
		}
		return els[i][1] < els[j][1]
	})
	return els
}

func TestMariaDBUnreachableBeforeEnable(t *testing.T) {
	ctx := ctxService(t)
	g := grantFixture(t)
	if out, err := wsm(ctx, "project", "block", "list", g.a.slug); err != nil || !strings.Contains(out, "mariadb") {
		t.Fatalf("precondition: iso-m has no mariadb ceiling: %v %s", err, out)
	}
	if n := countElements(svcElements(ctx, t), g.a.uid, 0); n != 0 {
		t.Fatalf("precondition: iso-m has %d elements", n)
	}
	for _, tag := range []string{tagOld, tagNew} {
		port := instancePort(ctx, t, tag)
		blockedBy(t, "wsm0_blocked", "iso-m/main to "+tag+" before service_enable", func() (bool, string) {
			return tcpFromSandbox(ctx, g.a, wsm0IP, port)
		})
	}
}

func TestServiceEnableGivesWorkingAccount(t *testing.T) {
	ctx := ctxService(t)
	g := grantFixture(t)
	portOld, portNew := instancePort(ctx, t, tagOld), instancePort(ctx, t, tagNew)
	c := enable(ctx, t, g.a, map[string]any{"block": "mariadb"})
	user, appDB := "sbx_"+g.a.sandboxID, "sbx_"+g.a.sandboxID+"_app"
	if c.Block != "mariadb" || c.Tag != tagOld || c.Host != wsm0IP || c.Port != portOld || c.User != user ||
		c.Database != appDB || len(c.Password) != 43 {
		t.Errorf("credentials: block %s tag %s host %s port %d user %s database %s, password of %d characters",
			c.Block, c.Tag, c.Host, c.Port, c.User, c.Database, len(c.Password))
	}

	testDB := "sbx_" + g.a.sandboxID + "_test"
	r := sqlFromSandbox(ctx, t, g.a, c,
		"CREATE TABLE t (id INT PRIMARY KEY)",
		"INSERT INTO t VALUES (7)",
		"SELECT id FROM t",
		"CREATE DATABASE "+testDB,
		"CREATE DATABASE other_x",
		"SHOW DATABASES",
		"SELECT user FROM mysql.user",
		"SET GLOBAL max_connections = 10",
		"SELECT LOAD_FILE('/etc/passwd')",
	)
	if r.ConnectError != "" {
		t.Fatalf("first connect failed: %s (%d)", r.ConnectError, r.ConnectErrno)
	}
	if len(r.Results) != 9 {
		t.Fatalf("results: %+v", r.Results)
	}
	for i := 0; i < 4; i++ {
		if !r.Results[i].OK {
			t.Errorf("statement %d failed: errno %d", i, r.Results[i].Errno)
		}
	}
	if fmt.Sprint(r.Results[2].Rows) != "[[7]]" {
		t.Errorf("SELECT from own table: %v", r.Results[2].Rows)
	}
	if r.Results[4].OK || r.Results[4].Errno != 1044 {
		t.Errorf("CREATE DATABASE other_x: ok %v errno %d, want 1044", r.Results[4].OK, r.Results[4].Errno)
	}
	var dbs []string
	for _, row := range r.Results[5].Rows {
		dbs = append(dbs, fmt.Sprint(row[0]))
	}
	sort.Strings(dbs)
	if want := []string{"information_schema", appDB, testDB}; !reflect.DeepEqual(dbs, want) {
		t.Errorf("SHOW DATABASES = %v, want %v", dbs, want)
	}
	for i, want := range map[int]int{6: 1142, 7: 1227} {
		if r.Results[i].OK || r.Results[i].Errno != want {
			t.Errorf("statement %d: ok %v errno %d, want %d", i, r.Results[i].OK, r.Results[i].Errno, want)
		}
	}
	if !r.Results[8].OK || fmt.Sprint(r.Results[8].Rows) != "[[<nil>]]" {
		t.Errorf("LOAD_FILE: ok %v rows %v, want NULL", r.Results[8].OK, r.Results[8].Rows)
	}

	// A second call returns the same credentials.
	if c2 := enable(ctx, t, g.a, map[string]any{"block": "mariadb"}); c2 != c {
		t.Error("a second service_enable returned other credentials")
	}

	// services_available and sandbox_info show the grant, no password.
	for _, tool := range []string{"services_available", "sandbox_info"} {
		r := mcpFromSandbox(ctx, t, g.a, mcpRequest{Op: "call", Tool: tool})
		if r.Error != "" || r.IsError {
			t.Fatalf("%s: %s %s", tool, r.Error, r.Text)
		}
		out := r.Text + string(r.Structured)
		if strings.Contains(out, c.Password) || strings.Contains(strings.ToLower(out), "password") {
			t.Errorf("%s shows a password", tool)
		}
		switch tool {
		case "services_available":
			if !strings.Contains(r.Text, `"enabled_tags":["11.8"]`) || !strings.Contains(r.Text, `"default_tag":"11.8"`) {
				t.Errorf("services_available = %s", r.Text)
			}
		case "sandbox_info":
			if !strings.Contains(r.Text, fmt.Sprintf(`"tag":"11.8","host":"10.255.0.1","port":%d,"user":"%s","database":"%s"`, portOld, user, appDB)) {
				t.Errorf("sandbox_info = %s", r.Text)
			}
		}
	}

	// Tag 12.3: a second grant on the other port.
	c3 := enable(ctx, t, g.a, map[string]any{"block": "mariadb", "tag": tagNew})
	if c3.Tag != tagNew || c3.Port != portNew || c3.User != user || c3.Password == c.Password {
		t.Errorf("12.3: tag %s port %d user %s, same password %v", c3.Tag, c3.Port, c3.User, c3.Password == c.Password)
	}
	r = sqlFromSandbox(ctx, t, g.a, c3, "SELECT VERSION()")
	if r.ConnectError != "" || len(r.Results) != 1 || !strings.HasPrefix(fmt.Sprint(r.Results[0].Rows), "[["+tagNew+".") {
		t.Errorf("12.3 from the sandbox: %s %+v", r.ConnectError, r.Results)
	}
	if n := grantRows(ctx, t, g.a.sandboxID); n != 2 {
		t.Errorf("grant rows of iso-m/main: %d, want 2", n)
	}
}

func TestServiceEnableOutsideCeilingDenied(t *testing.T) {
	ctx := ctxService(t)
	g := grantFixture(t)
	rowsBefore := grantRows(ctx, t, "")
	elsBefore := sortedPairs(ctx, t)
	before := auditHead(ctx, t)
	cases := []struct {
		name string
		p    project
		args map[string]any
	}{
		{"B without ceiling", g.b, map[string]any{"block": "mariadb"}},
		{"A with tag 11.4", g.a, map[string]any{"block": "mariadb", "tag": "11.4"}},
		{"A with redis", g.a, map[string]any{"block": "redis"}},
		{"A with a project argument", g.a, map[string]any{"block": "mariadb", "project": "iso-n"}},
		{"A with a quote in the tag", g.a, map[string]any{"block": "mariadb", "tag": "11.8'; DROP USER root;--"}},
	}
	for _, tc := range cases {
		r := mcpFromSandbox(ctx, t, tc.p, mcpRequest{Op: "call", Tool: "service_enable", Arguments: tc.args})
		if r.Error != "" || !r.IsError || !strings.HasPrefix(r.Text, "denied:") {
			t.Errorf("%s: error %q isError %v text %q", tc.name, r.Error, r.IsError, r.Text)
		}
	}
	n := 0
	for _, ev := range auditAfter(ctx, t, before) {
		if ev.Tool == "service_enable" {
			if ev.Result != "denied" {
				t.Errorf("event %+v, want denied", ev)
			}
			n++
		}
	}
	if n != len(cases) {
		t.Errorf("%d service_enable events, want %d", n, len(cases))
	}
	if after := grantRows(ctx, t, ""); after != rowsBefore {
		t.Errorf("grant rows %d -> %d", rowsBefore, after)
	}
	if after := sortedPairs(ctx, t); !reflect.DeepEqual(after, elsBefore) {
		t.Errorf("svc_allow4 changed: %v -> %v", elsBefore, after)
	}
}

func TestOtherProjectCannotUseGrant(t *testing.T) {
	ctx := ctxService(t)
	g := grantFixture(t)
	portOld, portNew := instancePort(ctx, t, tagOld), instancePort(ctx, t, tagNew)
	ca := enable(ctx, t, g.a, map[string]any{"block": "mariadb"})

	// B with A's credentials: the network stops it first.
	blockedBy(t, "wsm0_blocked", "iso-n/main to 11.8", func() (bool, string) {
		return tcpFromSandbox(ctx, g.b, wsm0IP, portOld)
	})
	if r := sqlFromSandbox(ctx, t, g.b, ca, "SELECT 1"); r.ConnectError == "" {
		t.Error("iso-n/main logged in with the credentials of iso-m")
	}

	// B gets its own ceiling and grant on the same instance.
	if out, err := wsm(ctx, "project", "block", "set", g.b.slug, "mariadb", "--tag", tagOld); err != nil {
		t.Fatalf("block set iso-n: %v: %s", err, out)
	}
	cb := enable(ctx, t, g.b, map[string]any{"block": "mariadb"})
	if cb.Port != portOld || cb.User == ca.User {
		t.Errorf("iso-n grant: port %d user %s", cb.Port, cb.User)
	}
	aApp := "sbx_" + g.a.sandboxID + "_app"
	r := sqlFromSandbox(ctx, t, g.b, cb,
		"SHOW DATABASES",
		"USE "+aApp,
		"CREATE DATABASE sbx_"+g.a.sandboxID+"_evil",
		"SELECT id FROM "+aApp+".t",
	)
	if r.ConnectError != "" || len(r.Results) != 4 {
		t.Fatalf("iso-n with its own credentials: %s %+v", r.ConnectError, r.Results)
	}
	for _, row := range r.Results[0].Rows {
		if name := fmt.Sprint(row[0]); name != "information_schema" && !strings.HasPrefix(name, "sbx_"+g.b.sandboxID+"_") {
			t.Errorf("iso-n sees database %s", name)
		}
	}
	for i := 1; i <= 2; i++ {
		if r.Results[i].OK || r.Results[i].Errno != 1044 {
			t.Errorf("statement %d: ok %v errno %d, want 1044", i, r.Results[i].OK, r.Results[i].Errno)
		}
	}
	if r.Results[3].OK {
		t.Error("iso-n read a table of iso-m")
	}

	want := [][2]int{{g.a.uid, portOld}, {g.a.uid, portNew}, {g.b.uid, portOld}}
	sort.Slice(want, func(i, j int) bool {
		if want[i][0] != want[j][0] {
			return want[i][0] < want[j][0]
		}
		return want[i][1] < want[j][1]
	})
	if got := sortedPairs(ctx, t); !reflect.DeepEqual(got, want) {
		t.Errorf("svc_allow4 = %v, want %v", got, want)
	}
}

func TestSandboxDeleteRevokesGrant(t *testing.T) {
	ctx := ctxService(t)
	g := grantFixture(t)
	portOld, portNew := instancePort(ctx, t, tagOld), instancePort(ctx, t, tagNew)
	enable(ctx, t, g.a, map[string]any{"block": "mariadb"})
	c2 := enable(ctx, t, g.a2, map[string]any{"block": "mariadb"})
	if r := sqlFromSandbox(ctx, t, g.a2, c2, "SELECT 1"); r.ConnectError != "" {
		t.Fatalf("control: iso-m/two cannot log in: %s", r.ConnectError)
	}

	if out, err := wsm(ctx, "sandbox", "delete", g.a.slug+"/main"); err != nil {
		t.Fatalf("delete iso-m/main: %v: %s", err, out)
	}
	acct := "sbx_" + g.a.sandboxID
	for _, tag := range []string{tagOld, tagNew} {
		if u := rootSQL(ctx, t, tag, "SELECT user FROM mysql.user WHERE user = ?", acct); len(u) != 0 {
			t.Errorf("%s: account %s still exists", tag, acct)
		}
		if dbs := rootSQL(ctx, t, tag, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ?",
			`sbx\_`+g.a.sandboxID+`\_%`); len(dbs) != 0 {
			t.Errorf("%s: databases left: %v", tag, dbs)
		}
	}
	if n := grantRows(ctx, t, g.a.sandboxID); n != 0 {
		t.Errorf("grant rows of iso-m/main: %d", n)
	}
	els := svcElements(ctx, t)
	if countElements(els, g.a.uid, portOld) != 1 {
		t.Error("the element of iso-m on 11.8 went, although iso-m/two still has a grant")
	}
	if countElements(els, g.a.uid, portNew) != 0 {
		t.Error("the element of iso-m on 12.3 stayed after its last grant went")
	}
	if r := sqlFromSandbox(ctx, t, g.a2, c2, "SELECT 1"); r.ConnectError != "" {
		t.Errorf("iso-m/two lost its access: %s", r.ConnectError)
	}

	if out, err := wsm(ctx, "sandbox", "delete", g.a.slug+"/two"); err != nil {
		t.Fatalf("delete iso-m/two: %v: %s", err, out)
	}
	if n := countElements(svcElements(ctx, t), g.a.uid, 0); n != 0 {
		t.Errorf("iso-m has %d elements after its last sandbox went", n)
	}
	if u := rootSQL(ctx, t, tagOld, "SELECT user FROM mysql.user WHERE user = ?", "sbx_"+g.a2.sandboxID); len(u) != 0 {
		t.Error("the account of iso-m/two still exists")
	}
	blockedBy(t, "wsm0_blocked", "project user iso-m to 11.8 after the deletes", func() (bool, string) {
		return probeAs(ctx, g.a.user, fmt.Sprintf("tcp:%s:%d", wsm0IP, portOld))
	})
}

func TestGrantsRestoredAfterRestart(t *testing.T) {
	ctx := ctxService(t)
	g := grantFixture(t)
	port := instancePort(ctx, t, tagOld)
	cb := enable(ctx, t, g.b, map[string]any{"block": "mariadb"})
	if out, err := run(ctx, "systemctl", "restart", "wsm.service"); err != nil {
		t.Fatalf("restart wsm: %v: %s", err, out)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := s.api.Projects(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the API did not come back after the restart")
		}
		time.Sleep(time.Second)
	}
	if n := countElements(svcElements(ctx, t), g.b.uid, port); n != 1 {
		t.Fatalf("iso-n has %d elements on 11.8 after the restart, want 1", n)
	}
	deadline = time.Now().Add(30 * time.Second)
	for {
		r := sqlFromSandbox(ctx, t, g.b, cb, "SELECT 1")
		if r.ConnectError == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("iso-n/main cannot log in after the restart: %s", r.ConnectError)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestServiceEnableAudited(t *testing.T) {
	ctx := ctxService(t)
	g := grantFixture(t)
	enable(ctx, t, g.b, map[string]any{"block": "mariadb"})

	// Every grant password: the ones seen and the ones stored now.
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
	rows, err := sqlDB.QueryContext(ctx, `SELECT id, secret_enc FROM service_grants`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var sealed []byte
		if err := rows.Scan(&id, &sealed); err != nil {
			t.Fatal(err)
		}
		pw, err := box.Open(sealed, []byte(id))
		if err != nil {
			t.Fatalf("open grant %s: %v", id, err)
		}
		seenPasswords.Store(string(pw), true)
	}
	rows.Close()
	var pws []string
	seenPasswords.Range(func(k, _ any) bool { pws = append(pws, k.(string)); return true })
	if len(pws) < 3 {
		t.Fatalf("only %d grant passwords to look for", len(pws))
	}
	leak := func(where string, data []byte) {
		for _, pw := range pws {
			if bytes.Contains(data, []byte(pw)) {
				t.Errorf("a grant password is in %s", where)
				return
			}
		}
	}

	var okN, deniedN int
	evRows, err := sqlDB.QueryContext(ctx, `SELECT result, detail, args_json FROM audit_events WHERE tool = 'service_enable'`)
	if err != nil {
		t.Fatal(err)
	}
	for evRows.Next() {
		var result, detail, args string
		if err := evRows.Scan(&result, &detail, &args); err != nil {
			t.Fatal(err)
		}
		switch result {
		case "ok":
			okN++
			if !strings.HasPrefix(detail, "granted mariadb ") {
				t.Errorf("ok event with detail %q", detail)
			}
		case "denied":
			deniedN++
		}
		leak("audit_events", []byte(detail+args))
	}
	evRows.Close()
	if okN == 0 || deniedN == 0 {
		t.Errorf("service_enable events: %d ok, %d denied", okN, deniedN)
	}

	for _, unit := range []string{"wsm", "wsm-helper"} {
		cmd := exec.CommandContext(ctx, "journalctl", "--no-pager", "-o", "cat", "-u", unit)
		cmd.Dir = "/"
		journal, err := cmd.Output()
		if err != nil || (unit == "wsm" && len(journal) == 0) {
			t.Errorf("journalctl -u %s: %v", unit, err)
		}
		leak("journalctl -u "+unit, journal)
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		if data, err := os.ReadFile(p); err == nil {
			leak(p, data)
		}
	}
	for _, args := range [][]string{{"service", "list"}, {"project", "block", "list", g.a.slug},
		{"project", "block", "list", g.b.slug}, {"sandbox", "list"}} {
		out, err := wsm(ctx, args...)
		if err != nil {
			t.Errorf("wsm %v: %v", args, err)
		}
		leak(fmt.Sprintf("wsm %v", args), []byte(out))
	}
	if out, err := wsm(ctx, "audit", "verify"); err != nil {
		t.Errorf("wsm audit verify: %v: %s", err, out)
	}
}

func TestCeilingShrinkRefusedWhileGranted(t *testing.T) {
	ctx := ctxService(t)
	g := grantFixture(t)
	enable(ctx, t, g.b, map[string]any{"block": "mariadb"})
	if out, err := wsm(ctx, "project", "block", "set", g.b.slug, "mariadb", "--tag", tagOld, "--tag", tagNew); err != nil {
		t.Fatalf("growing the ceiling: %v: %s", err, out)
	}
	before, err := wsm(ctx, "project", "block", "list", g.b.slug)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"project", "block", "remove", g.b.slug, "mariadb"},
		{"project", "block", "set", g.b.slug, "mariadb", "--tag", tagNew},
	} {
		out, err := wsm(ctx, args...)
		if err == nil || !strings.Contains(out, "main") {
			t.Errorf("wsm %v: %v: %s; want a failure naming the sandbox", args, err, out)
		}
	}
	for _, req := range []struct{ method, body string }{
		{http.MethodDelete, ""},
		{http.MethodPut, `{"tags":["12.3"]}`},
	} {
		code, body := apiStatus(ctx, t, req.method, "/v1/projects/"+g.b.slug+"/blocks/mariadb", req.body)
		if code != http.StatusConflict || !strings.Contains(body, "main") {
			t.Errorf("%s: %d %s, want 409 naming the sandbox", req.method, code, body)
		}
	}
	if after, err := wsm(ctx, "project", "block", "list", g.b.slug); err != nil || after != before {
		t.Errorf("ceiling changed:\n%s\nwas:\n%s", after, before)
	}
}
