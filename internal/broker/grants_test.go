package broker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/JCombee/wsm/internal/db"
)

const testID = "abcdefgh2345"
const testPW = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJ0123-_x"

// fakeSQL is a fake root session. Statements go to the helper's event log
// as "sql <port> <statement>".
type fakeSQL struct {
	h *fakeHelper

	mu sync.Mutex
	// down makes connecting fail; failExec makes every Exec fail.
	down     map[int]bool
	failExec bool
	// sessions and schemas answer the information_schema queries.
	sessions []string
	schemas  []string
	// killGone makes KILL answer 1094.
	killGone bool
	queries  []string // the args of the queries
}

func (f *fakeSQL) connect(_ context.Context, port int, password string) (SQLConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[port] {
		return nil, errors.New("connection refused")
	}
	if len(password) != 43 {
		return nil, errors.New("bad root password")
	}
	return &fakeConn{f: f, port: port}, nil
}

func (f *fakeSQL) set(fn func(f *fakeSQL)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type fakeConn struct {
	f    *fakeSQL
	port int
}

func (c *fakeConn) Exec(_ context.Context, query string, args ...any) error {
	c.f.mu.Lock()
	fail, gone := c.f.failExec, c.f.killGone
	c.f.mu.Unlock()
	if fail {
		return errors.New("server has gone away")
	}
	c.f.h.event(fmt.Sprintf("sql %d %s", c.port, query))
	if gone && strings.HasPrefix(query, "KILL") {
		return &mysql.MySQLError{Number: 1094, Message: "Unknown thread id"}
	}
	return nil
}

func (c *fakeConn) Strings(_ context.Context, query string, args ...any) ([]string, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if c.f.failExec {
		return nil, errors.New("server has gone away")
	}
	c.f.queries = append(c.f.queries, fmt.Sprint(args...))
	switch {
	case strings.Contains(query, "PROCESSLIST"):
		return c.f.sessions, nil
	case strings.Contains(query, "SCHEMATA"):
		return c.f.schemas, nil
	}
	return nil, fmt.Errorf("unexpected query %q", query)
}

func (c *fakeConn) Close() error { return nil }

func TestAccountSQL(t *testing.T) {
	stmts, err := createAccountSQL(testID, testPW)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE USER IF NOT EXISTS 'sbx_abcdefgh2345'@'%' IDENTIFIED BY '" + testPW + "' WITH MAX_USER_CONNECTIONS 50",
		"ALTER USER 'sbx_abcdefgh2345'@'%' IDENTIFIED BY '" + testPW + "' WITH MAX_USER_CONNECTIONS 50",
		"GRANT ALL PRIVILEGES ON `sbx\\_abcdefgh2345\\_%`.* TO 'sbx_abcdefgh2345'@'%'",
		"CREATE DATABASE IF NOT EXISTS `sbx_abcdefgh2345_app`",
	}
	if !reflect.DeepEqual(stmts, want) {
		t.Errorf("createAccountSQL =\n%q\nwant\n%q", stmts, want)
	}

	for _, tt := range []struct{ name, id, pw string }{
		{"short password", testID, testPW[:42]},
		{"long password", testID, testPW + "a"},
		{"quote in password", testID, testPW[:42] + "'"},
		{"space in password", testID, testPW[:42] + " "},
		{"backslash in password", testID, testPW[:42] + `\`},
		{"bad id", "abc'; DROP", testPW},
		{"upper id", "ABCDEFGH2345", testPW},
		{"empty id", "", testPW},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := createAccountSQL(tt.id, tt.pw); !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			h := newFakeHelper()
			f := &fakeSQL{h: h}
			conn, _ := f.connect(context.Background(), 20000, testPW)
			if err := createAccount(context.Background(), conn, tt.id, tt.pw); err == nil {
				t.Error("createAccount accepted it")
			}
			if tt.id != testID {
				if err := revokeAccount(context.Background(), conn, tt.id); err == nil {
					t.Error("revokeAccount accepted it")
				}
			}
			if ev := h.takeEvents(); len(ev) != 0 {
				t.Errorf("SQL ran before the check: %q", ev)
			}
		})
	}

	if got := quoteIdent("sbx_x_a`b"); got != "`sbx_x_a``b`" {
		t.Errorf("quoteIdent = %s", got)
	}
	if got := quoteIdent("``"); got != "``````" {
		t.Errorf("quoteIdent(``) = %s", got)
	}
	if got := likePattern(testID); got != `sbx\_abcdefgh2345\_%` {
		t.Errorf("likePattern = %s", got)
	}

	// Revoke: DROP USER, KILL each session, then DROP DATABASE per name,
	// quoted; names without the exact prefix are skipped; 1094 is fine.
	h := newFakeHelper()
	f := &fakeSQL{h: h, sessions: []string{"12", "40"}, killGone: true,
		schemas: []string{"sbx_abcdefgh2345_app", "sbx_abcdefgh2345_a`b", "SBX_abcdefgh2345_x"}}
	conn, _ := f.connect(context.Background(), 20000, testPW)
	if err := revokeAccount(context.Background(), conn, testID); err != nil {
		t.Fatalf("revokeAccount: %v", err)
	}
	wantEv := []string{
		"sql 20000 DROP USER IF EXISTS 'sbx_abcdefgh2345'@'%'",
		"sql 20000 KILL CONNECTION 12",
		"sql 20000 KILL CONNECTION 40",
		"sql 20000 DROP DATABASE IF EXISTS `sbx_abcdefgh2345_app`",
		"sql 20000 DROP DATABASE IF EXISTS `sbx_abcdefgh2345_a``b`",
	}
	if ev := h.takeEvents(); !reflect.DeepEqual(ev, wantEv) {
		t.Errorf("revoke statements =\n%q\nwant\n%q", ev, wantEv)
	}
	if want := []string{"sbx_abcdefgh2345", `sbx\_abcdefgh2345\_%`}; !reflect.DeepEqual(f.queries, want) {
		t.Errorf("query args = %q, want %q", f.queries, want)
	}
	// A session id that is not a number stops the revoke.
	f.set(func(f *fakeSQL) { f.sessions = []string{"1; DROP"} })
	if err := revokeAccount(context.Background(), conn, testID); err == nil {
		t.Error("non-numeric session id accepted")
	}

	// An error from the server never carries the password.
	err = createAccount(context.Background(), &echoConn{}, testID, testPW)
	if err == nil || strings.Contains(err.Error(), testPW) {
		t.Errorf("createAccount error leaks the password: %v", err)
	}
}

// echoConn fails every statement with an error that quotes it.
type echoConn struct{ fakeConn }

func (echoConn) Exec(_ context.Context, query string, _ ...any) error {
	return fmt.Errorf("Error 1064: syntax error near '%s'", query)
}

// grantEnv is a broker with two projects, a ceiling for project a and
// fake SQL.
type grantEnv struct {
	*testEnv
	sql *fakeSQL
	pa  db.Project
	pb  db.Project
}

func newGrantEnv(t *testing.T) *grantEnv {
	t.Helper()
	e := newEnv(t)
	f := &fakeSQL{h: e.h, down: map[int]bool{}}
	e.b.SQL = f.connect
	e.b.GrantWait = 100 * time.Millisecond
	ctx := context.Background()
	if _, err := e.b.SetBlock(ctx, "a", "mariadb", "", []string{"11.8", "12.3"}); err != nil {
		t.Fatal(err)
	}
	pa, _ := e.s.ProjectBySlug(ctx, "a")
	pb, _ := e.s.ProjectBySlug(ctx, "b")
	e.h.takeEvents()
	return &grantEnv{testEnv: e, sql: f, pa: pa, pb: pb}
}

func (e *grantEnv) sandbox(t *testing.T, p db.Project, slug string) db.Sandbox {
	t.Helper()
	sb := &db.Sandbox{ProjectID: p.ID, Slug: slug,
		MCPTokenHash: fmt.Sprintf("%x", sha256.Sum256([]byte(p.Slug+"/"+slug)))}
	if err := e.s.CreateSandbox(context.Background(), sb, func(id string) string { return "sbx-" + id }); err != nil {
		t.Fatal(err)
	}
	return *sb
}

func (e *grantEnv) port(t *testing.T, tag string) int {
	t.Helper()
	inst, err := e.s.ServiceInstance(context.Background(), "mariadb", tag)
	if err != nil {
		t.Fatal(err)
	}
	return inst.Port
}

func TestEnableWithinCeiling(t *testing.T) {
	ctx := context.Background()
	e := newGrantEnv(t)
	sa := e.sandbox(t, e.pa, "one")
	sbB := e.sandbox(t, e.pb, "one")
	p118, p123 := e.port(t, "11.8"), e.port(t, "12.3")

	// Denials.
	for _, tt := range []struct {
		name, sandbox, block, tag string
	}{
		{"unknown block", sa.ID, "nosuchblock", ""},
		{"no such block", sa.ID, "postgres", ""},
		{"tag outside ceiling", sa.ID, "mariadb", "11.4"},
		{"project without ceiling", sbB.ID, "mariadb", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.b.Enable(ctx, tt.sandbox, tt.block, tt.tag)
			if !errors.Is(err, ErrDenied) {
				t.Errorf("err = %v, want ErrDenied", err)
			}
		})
	}
	if ev := e.h.takeEvents(); len(ev) != 0 {
		t.Errorf("denials ran something: %q", ev)
	}

	// The default tag is the first of the ceiling.
	c, err := e.b.Enable(ctx, sa.ID, "mariadb", "")
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	want := Credentials{Block: "mariadb", Tag: "11.8", Host: "10.255.0.1", Port: p118,
		User: "sbx_" + sa.ID, Password: c.Password, Database: "sbx_" + sa.ID + "_app"}
	if c != want || len(c.Password) != 43 {
		t.Errorf("Enable = %+v, want %+v", c, want)
	}
	// The account SQL runs, then AllowService, before the credentials.
	ev := e.h.takeEvents()
	if len(ev) != 5 || !strings.HasPrefix(ev[0], fmt.Sprintf("sql %d CREATE USER", p118)) ||
		ev[4] != fmt.Sprintf("allow %d %d", e.pa.LinuxUID, p118) {
		t.Errorf("events = %q", ev)
	}

	// A second call returns the same credentials and repairs the account.
	c2, err := e.b.Enable(ctx, sa.ID, "mariadb", "11.8")
	if err != nil || c2 != c {
		t.Errorf("second Enable = %+v, %v; want the same credentials", c2, err)
	}
	if ev := e.h.takeEvents(); len(ev) != 5 {
		t.Errorf("second Enable events = %q", ev)
	}
	// Another tag: a second grant on the other port, with its own password.
	c3, err := e.b.Enable(ctx, sa.ID, "mariadb", "12.3")
	if err != nil || c3.Port != p123 || c3.User != c.User || c3.Password == c.Password {
		t.Errorf("Enable 12.3 = %+v, %v", c3, err)
	}
	e.h.takeEvents()
	if gs, _ := e.s.GrantsBySandbox(ctx, sa.ID); len(gs) != 2 {
		t.Errorf("grants = %d, want 2", len(gs))
	}

	// AllowService fails: no credentials; the row stays and a next call
	// succeeds with the same password.
	s2 := e.sandbox(t, e.pa, "two")
	e.h.mu.Lock()
	e.h.failAllow = true
	e.h.mu.Unlock()
	if c, err := e.b.Enable(ctx, s2.ID, "mariadb", ""); err == nil || c.Password != "" {
		t.Errorf("Enable with failing AllowService = %+v, %v", c, err)
	}
	e.h.mu.Lock()
	e.h.failAllow = false
	e.h.mu.Unlock()
	g, err := e.s.Grant(ctx, s2.ID, mustInstance(t, e.s, "11.8").ID)
	if err != nil {
		t.Fatalf("grant row after failed allow: %v", err)
	}
	c4, err := e.b.Enable(ctx, s2.ID, "mariadb", "")
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := e.box.Open(g.SecretEnc, []byte(g.ID)); string(pw) != c4.Password {
		t.Error("retry did not reuse the stored password")
	}
	// Failing SQL: no credentials, no AllowService.
	e.h.takeEvents()
	e.sql.set(func(f *fakeSQL) { f.failExec = true })
	if c, err := e.b.Enable(ctx, s2.ID, "mariadb", ""); err == nil || c.Password != "" {
		t.Errorf("Enable with failing SQL = %+v, %v", c, err)
	}
	e.sql.set(func(f *fakeSQL) { f.failExec = false })
	for _, ev := range e.h.takeEvents() {
		if strings.HasPrefix(ev, "allow") {
			t.Errorf("AllowService after failing SQL: %s", ev)
		}
	}

	// An instance that does not answer in time.
	e.sql.set(func(f *fakeSQL) { f.down[p123] = true })
	start := time.Now()
	if _, err := e.b.Enable(ctx, s2.ID, "mariadb", "12.3"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("down instance: err = %v, want ErrUnavailable", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Error("Enable did not wait for the instance")
	}
	if _, err := e.s.Grant(ctx, s2.ID, mustInstance(t, e.s, "12.3").ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("grant row for an unavailable instance: %v", err)
	}

	// A sandbox without token hash is being deleted.
	if err := e.s.ClearMCPToken(ctx, s2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.Enable(ctx, s2.ID, "mariadb", ""); !errors.Is(err, ErrDenied) {
		t.Errorf("sandbox without token: err = %v, want ErrDenied", err)
	}

	// Available and Enabled show the grants without passwords.
	offers, err := e.b.Available(ctx, sa.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantOffers := []Offer{{Block: "mariadb", Profile: "dev", Tags: []string{"11.8", "12.3"}, DefaultTag: "11.8",
		EnabledTags: []string{"11.8", "12.3"}}}
	if !reflect.DeepEqual(offers, wantOffers) {
		t.Errorf("Available = %+v, want %+v", offers, wantOffers)
	}
	en, err := e.b.Enabled(ctx, sa.ID)
	if err != nil || len(en) != 2 || en[0] != (Enabled{Block: "mariadb", Tag: "11.8", Host: "10.255.0.1",
		Port: p118, User: c.User, Database: c.Database}) {
		t.Errorf("Enabled = %+v, %v", en, err)
	}
	if strings.Contains(fmt.Sprintf("%+v %+v", offers, en), c.Password) {
		t.Error("Available or Enabled shows a password")
	}
	if strings.Contains(e.logs.String(), c.Password) || strings.Contains(e.logs.String(), c3.Password) {
		t.Error("the log contains a password")
	}
}

func mustInstance(t *testing.T, s *db.Store, tag string) db.ServiceInstance {
	t.Helper()
	inst, err := s.ServiceInstance(context.Background(), "mariadb", tag)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestRevokeSandbox(t *testing.T) {
	ctx := context.Background()
	e := newGrantEnv(t)
	one := e.sandbox(t, e.pa, "one")
	two := e.sandbox(t, e.pa, "two")
	p118, p123 := e.port(t, "11.8"), e.port(t, "12.3")
	for _, g := range []struct {
		sb  db.Sandbox
		tag string
	}{{one, "11.8"}, {one, "12.3"}, {two, "11.8"}} {
		if _, err := e.b.Enable(ctx, g.sb.ID, "mariadb", g.tag); err != nil {
			t.Fatal(err)
		}
	}
	e.h.takeEvents()
	e.sql.set(func(f *fakeSQL) { f.sessions = []string{"7"}; f.schemas = []string{"sbx_" + one.ID + "_app"} })

	// The instance of 12.3 is down: 11.8 is revoked, then it stops; the
	// 12.3 row stays.
	e.sql.set(func(f *fakeSQL) { f.down[p123] = true })
	if err := e.b.RevokeSandbox(ctx, one.ID); err == nil {
		t.Fatal("RevokeSandbox with a down instance succeeded")
	}
	ev := e.h.takeEvents()
	wantEv := []string{
		fmt.Sprintf("sql %d DROP USER IF EXISTS 'sbx_%s'@'%%'", p118, one.ID),
		fmt.Sprintf("sql %d KILL CONNECTION 7", p118),
		fmt.Sprintf("sql %d DROP DATABASE IF EXISTS `sbx_%s_app`", p118, one.ID),
		// sandbox two still has a grant on 11.8: no revoke of the element.
	}
	if !reflect.DeepEqual(ev, wantEv) {
		t.Errorf("events =\n%q\nwant\n%q", ev, wantEv)
	}
	gs, _ := e.s.GrantsBySandbox(ctx, one.ID)
	if len(gs) != 1 || gs[0].Tag != "12.3" {
		t.Errorf("grants left = %+v, want only 12.3", gs)
	}

	// A second attempt finishes; 12.3 was the project's last grant there.
	e.sql.set(func(f *fakeSQL) { f.down[p123] = false })
	if err := e.b.RevokeSandbox(ctx, one.ID); err != nil {
		t.Fatalf("second RevokeSandbox: %v", err)
	}
	ev = e.h.takeEvents()
	if len(ev) != 4 || ev[3] != fmt.Sprintf("revoke %d %d", e.pa.LinuxUID, p123) {
		t.Errorf("events = %q", ev)
	}
	if gs, _ := e.s.GrantsBySandbox(ctx, one.ID); len(gs) != 0 {
		t.Errorf("grants left = %+v", gs)
	}
	if err := e.s.DeleteSandbox(ctx, one.ID); err != nil {
		t.Errorf("DeleteSandbox after revoke: %v", err)
	}

	// The last grant of the project on 11.8: the element goes too, after
	// the account and before the row.
	e.h.mu.Lock()
	e.h.failRevoke = true
	e.h.mu.Unlock()
	if err := e.b.RevokeSandbox(ctx, two.ID); err == nil {
		t.Error("RevokeSandbox with a failing RevokeService succeeded")
	}
	if gs, _ := e.s.GrantsBySandbox(ctx, two.ID); len(gs) != 1 {
		t.Errorf("row gone although RevokeService failed: %+v", gs)
	}
	e.h.mu.Lock()
	e.h.failRevoke = false
	e.h.mu.Unlock()
	e.h.takeEvents()
	e.sql.set(func(f *fakeSQL) { f.schemas = []string{"sbx_" + two.ID + "_app"} })
	if err := e.b.RevokeSandbox(ctx, two.ID); err != nil {
		t.Fatal(err)
	}
	ev = e.h.takeEvents()
	if len(ev) != 4 || ev[3] != fmt.Sprintf("revoke %d %d", e.pa.LinuxUID, p118) {
		t.Errorf("events = %q", ev)
	}
	// Nothing left: a no-op.
	if err := e.b.RevokeSandbox(ctx, two.ID); err != nil {
		t.Errorf("RevokeSandbox without grants: %v", err)
	}
	if ev := e.h.takeEvents(); len(ev) != 0 {
		t.Errorf("no-op revoke ran %q", ev)
	}
}

func TestRestoreGrants(t *testing.T) {
	ctx := context.Background()
	e := newGrantEnv(t)
	if _, err := e.b.SetBlock(ctx, "b", "mariadb", "", []string{"11.8"}); err != nil {
		t.Fatal(err)
	}
	p118, p123 := e.port(t, "11.8"), e.port(t, "12.3")
	a1, a2, b1 := e.sandbox(t, e.pa, "one"), e.sandbox(t, e.pa, "two"), e.sandbox(t, e.pb, "one")
	for _, g := range []struct {
		sb  db.Sandbox
		tag string
	}{{a1, "11.8"}, {a2, "11.8"}, {a1, "12.3"}, {b1, "11.8"}} {
		if _, err := e.b.Enable(ctx, g.sb.ID, "mariadb", g.tag); err != nil {
			t.Fatal(err)
		}
	}
	e.h.takeEvents()
	n, err := e.b.RestoreGrants(ctx)
	if err != nil || n != 0 {
		t.Fatalf("RestoreGrants = %d, %v", n, err)
	}
	want := []string{
		fmt.Sprintf("allow %d %d", e.pa.LinuxUID, p118),
		fmt.Sprintf("allow %d %d", e.pa.LinuxUID, p123),
		fmt.Sprintf("allow %d %d", e.pb.LinuxUID, p118),
	}
	if ev := e.h.takeEvents(); !reflect.DeepEqual(ev, want) {
		t.Errorf("events = %q, want %q", ev, want)
	}

	// Errors are counted and the rest goes on.
	e.h.mu.Lock()
	e.h.failAllow = true
	e.h.mu.Unlock()
	n, err = e.b.RestoreGrants(ctx)
	if err != nil || n != 3 {
		t.Errorf("RestoreGrants with failures = %d, %v; want 3", n, err)
	}
	if c := strings.Count(e.logs.String(), "restore grant uid"); c != 3 {
		t.Errorf("logged %d failures, want 3", c)
	}
}

func TestCeilingShrinkWithGrants(t *testing.T) {
	ctx := context.Background()
	e := newGrantEnv(t)
	sa := e.sandbox(t, e.pa, "worker")
	if _, err := e.b.Enable(ctx, sa.ID, "mariadb", "12.3"); err != nil {
		t.Fatal(err)
	}
	before := e.ceiling(t, "a")
	pulls, ensures := e.h.counts()

	_, err := e.b.SetBlock(ctx, "a", "mariadb", "", []string{"11.8"})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "worker") {
		t.Errorf("SetBlock without the used tag: err = %v, want ErrConflict naming the sandbox", err)
	}
	if err := e.b.RemoveBlock(ctx, "a", "mariadb"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "worker") {
		t.Errorf("RemoveBlock: err = %v, want ErrConflict naming the sandbox", err)
	}
	if got := e.ceiling(t, "a"); got != before {
		t.Errorf("ceiling changed: %s, want %s", got, before)
	}
	if p, en := e.h.counts(); p != pulls || en != ensures {
		t.Error("a refused SetBlock pulled or ensured")
	}
	// Keeping the used tag, or adding one, is fine.
	if _, err := e.b.SetBlock(ctx, "a", "mariadb", "", []string{"12.3"}); err != nil {
		t.Errorf("SetBlock keeping 12.3: %v", err)
	}
	// Project b has no grants: shrinking and removing work.
	if _, err := e.b.SetBlock(ctx, "b", "mariadb", "", []string{"11.8", "12.3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.SetBlock(ctx, "b", "mariadb", "", []string{"11.8"}); err != nil {
		t.Errorf("SetBlock b: %v", err)
	}
	if err := e.b.RemoveBlock(ctx, "b", "mariadb"); err != nil {
		t.Errorf("RemoveBlock b: %v", err)
	}
	// Once the grant is revoked, a can remove the block.
	if err := e.b.RevokeSandbox(ctx, sa.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.b.RemoveBlock(ctx, "a", "mariadb"); err != nil {
		t.Errorf("RemoveBlock after revoke: %v", err)
	}
}
