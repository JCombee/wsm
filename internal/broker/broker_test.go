package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/secret"
)

// fakeHelper stands in for wsm-helper. An ensured instance starts
// answering with a greeting for its tag, unless the tag is in neverReady.
type fakeHelper struct {
	mu         sync.Mutex
	pulls      []string
	ensures    []string
	passwords  map[string][]string // tag -> passwords seen, in order
	failPull   map[string]bool
	failEnsure map[string]bool
	neverReady map[string]bool
	ready      map[int]string // port -> server version
	// events is the ordered log of AllowService, RevokeService and the
	// statements of fakeSQL.
	events     []string
	failAllow  bool
	failRevoke bool
}

func newFakeHelper() *fakeHelper {
	return &fakeHelper{passwords: map[string][]string{}, failPull: map[string]bool{},
		failEnsure: map[string]bool{}, neverReady: map[string]bool{}, ready: map[int]string{}}
}

func (f *fakeHelper) PullServiceImage(_ context.Context, service, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, service+" "+tag)
	if f.failPull[tag] {
		return errors.New("toomanyrequests")
	}
	if tag == "97.9" {
		return fmt.Errorf("helper pull_service_image: %w: manifest unknown", helper.ErrNotFound)
	}
	return nil
}

func (f *fakeHelper) EnsureServiceInstance(_ context.Context, service, tag string, port int, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, fmt.Sprintf("%s %s %d", service, tag, port))
	f.passwords[tag] = append(f.passwords[tag], password)
	if f.failEnsure[tag] {
		return errors.New("systemctl failed")
	}
	if !f.neverReady[tag] {
		f.ready[port] = tag + ".9-MariaDB-ubu2404"
	}
	return nil
}

func (f *fakeHelper) AllowService(_ context.Context, uid, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAllow {
		return errors.New("nft failed")
	}
	f.events = append(f.events, fmt.Sprintf("allow %d %d", uid, port))
	return nil
}

func (f *fakeHelper) RevokeService(_ context.Context, uid, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRevoke {
		return errors.New("nft failed")
	}
	f.events = append(f.events, fmt.Sprintf("revoke %d %d", uid, port))
	return nil
}

func (f *fakeHelper) event(e string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

// takeEvents returns the events so far and clears them.
func (f *fakeHelper) takeEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.events
	f.events = nil
	return out
}

func (f *fakeHelper) set(m map[string]bool, tag string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m[tag] = v
}

func (f *fakeHelper) counts() (pulls, ensures int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pulls), len(f.ensures)
}

// dial serves a greeting over a pipe for ready ports.
func (f *fakeHelper) dial(_ context.Context, port int) (net.Conn, error) {
	f.mu.Lock()
	v, ok := f.ready[port]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		server.Write(greetingPacket(0x0a, v, true))
	}()
	return client, nil
}

// greetingPacket builds a handshake packet with the given protocol byte
// and version, NUL-terminated or not, followed by some filler.
func greetingPacket(proto byte, version string, nul bool) []byte {
	payload := []byte{proto}
	payload = append(payload, version...)
	if nul {
		payload = append(payload, 0)
	}
	payload = append(payload, 1, 2, 3, 4, 'x', 'y', 0)
	n := len(payload)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), 0}, payload...)
}

type testEnv struct {
	b    *Broker
	h    *fakeHelper
	s    *db.Store
	logs *bytes.Buffer
	box  *secret.Box
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	s := db.NewStore(sqlDB)
	for _, slug := range []string{"a", "b"} {
		p := &db.Project{Slug: slug, Name: slug, ImageRef: "img", CPUMillis: 1, MemBytes: 1, PidsLimit: 1}
		if err := s.CreateProject(ctx, p, func(uid int) string { return fmt.Sprint("wsm-p-", uid) }); err != nil {
			t.Fatal(err)
		}
	}
	box, err := secret.New(bytes.Repeat([]byte{7}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	h := newFakeHelper()
	logs := &bytes.Buffer{}
	var logMu sync.Mutex
	b := &Broker{Store: s, Helper: h, Secrets: box, Addr: netip.MustParseAddr("10.255.0.1"),
		Dial: h.dial, ReadyTimeout: 200 * time.Millisecond, PollInterval: 10 * time.Millisecond,
		Logf: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(logs, format+"\n", args...)
		}}
	return &testEnv{b: b, h: h, s: s, logs: logs, box: box}
}

// dump returns every row of the tables the broker writes, as text.
func (e *testEnv) dump(t *testing.T) string {
	t.Helper()
	var out strings.Builder
	for _, table := range []string{"service_instances", "project_blocks", "jobs", "audit_events"} {
		rows, err := e.s.DB.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				fmt.Fprintf(&out, "%s|", v)
			}
		}
		rows.Close()
	}
	return out.String()
}

func (e *testEnv) ceiling(t *testing.T, slug string) string {
	t.Helper()
	cs, err := e.b.Blocks(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(cs)
}

// TestUnknownTagIsInvalid: a tag the registry does not know is invalid
// input and leaves no instance row; another pull error is not.
func TestUnknownTagIsInvalid(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, err := e.b.SetBlock(ctx, "a", "mariadb", "", []string{"97.9"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "tag 97.9 of block mariadb does not exist") {
		t.Errorf("unknown tag: err = %v, want ErrInvalid with the tag", err)
	}
	if _, err := e.s.ServiceInstance(ctx, "mariadb", "97.9"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("row for 97.9: %v", err)
	}
	e.h.set(e.h.failPull, "11.8", true)
	if _, err := e.b.SetBlock(ctx, "a", "mariadb", "", []string{"11.8"}); err == nil || errors.Is(err, ErrInvalid) {
		t.Errorf("other pull error: err = %v, want a non-invalid error", err)
	}
	if insts, _ := e.s.ServiceInstances(ctx); len(insts) != 0 {
		t.Errorf("instances after failed pulls: %+v", insts)
	}
}

func TestEnsureInstances(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	var errs []error

	// Two tags: one pull and one ensure each, on different ports.
	c, err := e.b.SetBlock(ctx, "a", "mariadb", "", []string{"11.8", "12.3"})
	if err != nil {
		t.Fatalf("SetBlock: %v", err)
	}
	if c.Profile != "dev" || strings.Join(c.Tags, ",") != "11.8,12.3" {
		t.Errorf("ceiling = %+v", c)
	}
	if got := strings.Join(e.h.pulls, ","); got != "mariadb 11.8,mariadb 12.3" {
		t.Errorf("pulls = %s", got)
	}
	if got := strings.Join(e.h.ensures, ","); got != "mariadb 11.8 20000,mariadb 12.3 20001" {
		t.Errorf("ensures = %s", got)
	}
	if got := e.ceiling(t, "a"); got != "[{mariadb dev [11.8 12.3]}]" {
		t.Errorf("stored ceiling = %s", got)
	}
	insts, err := e.s.ServiceInstances(ctx)
	if err != nil || len(insts) != 2 {
		t.Fatalf("instances = %v, %v", insts, err)
	}
	var passwords []string
	for _, inst := range insts {
		pw := e.h.passwords[inst.Tag][0]
		passwords = append(passwords, pw)
		if len(pw) != 43 || strings.ContainsAny(pw, "+/=") {
			t.Errorf("password of %s has the wrong form (%d characters)", inst.Tag, len(pw))
		}
		opened, err := e.box.Open(inst.SecretEnc, []byte(inst.ID))
		if err != nil || string(opened) != pw {
			t.Errorf("password of %s does not open with the instance ID", inst.Tag)
		}
		if _, err := e.box.Open(inst.SecretEnc, []byte("otherinstnce")); err == nil {
			t.Errorf("password of %s opens with another ID", inst.Tag)
		}
	}
	if passwords[0] == passwords[1] {
		t.Error("two instances share a password")
	}

	// The same tags again: nothing new to pull, both ensured again.
	pulls, ensures := e.h.counts()
	if _, err := e.b.SetBlock(ctx, "a", "mariadb", "dev", []string{"11.8", "12.3"}); err != nil {
		t.Fatal(err)
	}
	if p, en := e.h.counts(); p != pulls || en != ensures+2 {
		t.Errorf("second SetBlock: %d pulls, %d ensures; want %d, %d", p, en, pulls, ensures+2)
	}
	if e.h.passwords["11.8"][1] != passwords[0] {
		t.Error("the second ensure got another password")
	}
	// Another project shares the instance.
	if _, err := e.b.SetBlock(ctx, "b", "mariadb", "", []string{"11.8"}); err != nil {
		t.Fatal(err)
	}
	if insts, _ := e.s.ServiceInstances(ctx); len(insts) != 2 {
		t.Errorf("project b made a new instance: %d", len(insts))
	}

	// A failed pull leaves no row and no ceiling change.
	e.h.set(e.h.failPull, "99.9", true)
	_, err = e.b.SetBlock(ctx, "a", "mariadb", "", []string{"11.8", "99.9"})
	if err == nil {
		t.Fatal("SetBlock with a tag that does not exist succeeded")
	}
	errs = append(errs, err)
	if _, err := e.s.ServiceInstance(ctx, "mariadb", "99.9"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("row for 99.9: %v", err)
	}
	if got := e.ceiling(t, "a"); got != "[{mariadb dev [11.8 12.3]}]" {
		t.Errorf("ceiling changed after a failed pull: %s", got)
	}

	// A failed ensure keeps the row, not the ceiling; the next attempt
	// uses the same port and password.
	e.h.set(e.h.failEnsure, "11.4", true)
	_, err = e.b.SetBlock(ctx, "a", "mariadb", "", []string{"11.4"})
	if err == nil {
		t.Fatal("SetBlock with a failing ensure succeeded")
	}
	errs = append(errs, err)
	inst114, err := e.s.ServiceInstance(ctx, "mariadb", "11.4")
	if err != nil {
		t.Fatalf("row for 11.4 is gone: %v", err)
	}
	if got := e.ceiling(t, "a"); got != "[{mariadb dev [11.8 12.3]}]" {
		t.Errorf("ceiling changed after a failed ensure: %s", got)
	}
	e.h.set(e.h.failEnsure, "11.4", false)
	if _, err := e.b.SetBlock(ctx, "a", "mariadb", "", []string{"11.4"}); err != nil {
		t.Fatal(err)
	}
	if pws := e.h.passwords["11.4"]; len(pws) != 2 || pws[0] != pws[1] {
		t.Error("the retry used another password")
	}
	if again, _ := e.s.ServiceInstance(ctx, "mariadb", "11.4"); again.Port != inst114.Port || again.ID != inst114.ID {
		t.Errorf("the retry changed the instance: %+v, was %+v", again, inst114)
	}
	if got := e.ceiling(t, "a"); got != "[{mariadb dev [11.4]}]" {
		t.Errorf("ceiling after the retry: %s", got)
	}

	// An instance that does not come ready: the same.
	e.h.set(e.h.neverReady, "13.0", true)
	_, err = e.b.SetBlock(ctx, "a", "mariadb", "", []string{"13.0"})
	if err == nil {
		t.Fatal("SetBlock with an instance that never comes ready succeeded")
	}
	errs = append(errs, err)
	inst130, err := e.s.ServiceInstance(ctx, "mariadb", "13.0")
	if err != nil {
		t.Fatalf("row for 13.0 is gone: %v", err)
	}
	if got := e.ceiling(t, "a"); got != "[{mariadb dev [11.4]}]" {
		t.Errorf("ceiling changed while not ready: %s", got)
	}
	e.h.set(e.h.neverReady, "13.0", false)
	if _, err := e.b.SetBlock(ctx, "a", "mariadb", "", []string{"13.0"}); err != nil {
		t.Fatal(err)
	}
	if pws := e.h.passwords["13.0"]; len(pws) != 2 || pws[0] != pws[1] {
		t.Error("the retry after not ready used another password")
	}
	if again, _ := e.s.ServiceInstance(ctx, "mariadb", "13.0"); again.Port != inst130.Port {
		t.Errorf("the retry changed the port: %d, was %d", again.Port, inst130.Port)
	}

	// Invalid input and unknown projects.
	for _, tt := range []struct {
		slug, block, profile string
		tags                 []string
		want                 error
	}{
		{"a", "nosuchblock", "", []string{"11.8"}, ErrInvalid},
		{"a", "mariadb", "prod", []string{"11.8"}, ErrInvalid},
		{"a", "mariadb", "", []string{"latest"}, ErrInvalid},
		{"a", "mariadb", "", nil, ErrInvalid},
		{"nope", "mariadb", "", []string{"11.8"}, db.ErrNotFound},
	} {
		_, err := e.b.SetBlock(ctx, tt.slug, tt.block, tt.profile, tt.tags)
		if !errors.Is(err, tt.want) {
			t.Errorf("SetBlock(%s, %s, %q, %v) = %v, want %v", tt.slug, tt.block, tt.profile, tt.tags, err, tt.want)
		}
	}

	// Services shows live state; EnsureAll ensures every instance.
	svcs, err := e.b.Services(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 4 {
		t.Fatalf("Services = %+v", svcs)
	}
	for _, s := range svcs {
		if s.State != StateReady || !strings.HasPrefix(s.Version, s.Tag+".") || s.Address != fmt.Sprintf("10.255.0.1:%d", s.Port) {
			t.Errorf("service %+v", s)
		}
	}
	e.h.mu.Lock()
	delete(e.h.ready, 20000)
	e.h.mu.Unlock()
	svcs, _ = e.b.Services(ctx)
	if svcs[0].Port != 20000 || svcs[0].State != StateDown || svcs[0].Version != "" {
		t.Errorf("down service = %+v", svcs[0])
	}
	_, ensures = e.h.counts()
	e.b.EnsureAll(ctx)
	if _, en := e.h.counts(); en != ensures+4 {
		t.Errorf("EnsureAll made %d ensures, want 4", en-ensures)
	}

	if err := e.b.RemoveBlock(ctx, "a", "mariadb"); err != nil {
		t.Fatal(err)
	}
	if got := e.ceiling(t, "a"); got != "[]" {
		t.Errorf("ceiling after RemoveBlock: %s", got)
	}
	if err := e.b.RemoveBlock(ctx, "a", "mariadb"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("second RemoveBlock: %v", err)
	}
	if insts, _ := e.s.ServiceInstances(ctx); len(insts) != 4 {
		t.Errorf("RemoveBlock removed instances: %d left", len(insts))
	}

	// No password anywhere but in sealed form.
	dump := e.dump(t)
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	for tag, pws := range e.h.passwords {
		for _, pw := range pws {
			if strings.Contains(dump, pw) {
				t.Errorf("password of %s is in the database", tag)
			}
			if strings.Contains(e.logs.String(), pw) {
				t.Errorf("password of %s is in the log", tag)
			}
			for _, err := range errs {
				if strings.Contains(err.Error(), pw) {
					t.Errorf("password of %s is in an error", tag)
				}
			}
			if strings.Contains(fmt.Sprint(svcs), pw) {
				t.Errorf("password of %s is in the service list", tag)
			}
		}
	}
}

// greetingConn returns a conn whose peer writes data and then waits.
func greetingConn(data []byte, closeAfter bool) net.Conn {
	client, server := net.Pipe()
	go func() {
		if len(data) > 0 {
			server.Write(data)
		}
		if closeAfter {
			server.Close()
		}
	}()
	return client
}

func TestGreetingVersion(t *testing.T) {
	big := greetingPacket(0x0a, strings.Repeat("1", 2000), true)
	tests := []struct {
		name    string
		data    []byte
		close   bool
		tag     string
		version string // empty: readGreeting fails
		match   bool
	}{
		{"plain", greetingPacket(0x0a, "11.8.9-MariaDB-ubu2404", true), false, "11.8", "11.8.9-MariaDB-ubu2404", true},
		{"compat prefix", greetingPacket(0x0a, "5.5.5-10.11.6-MariaDB", true), false, "10.11", "5.5.5-10.11.6-MariaDB", true},
		{"exact patch", greetingPacket(0x0a, "11.8.9-MariaDB", true), false, "11.8.9", "11.8.9-MariaDB", true},
		{"exact version", greetingPacket(0x0a, "12.3", true), false, "12.3", "12.3", true},
		{"other minor", greetingPacket(0x0a, "11.80.1-MariaDB", true), false, "11.8", "11.80.1-MariaDB", false},
		{"other patch", greetingPacket(0x0a, "11.8.10-MariaDB", true), false, "11.8.1", "11.8.10-MariaDB", false},
		{"other version", greetingPacket(0x0a, "12.3.3-MariaDB", true), false, "11.8", "12.3.3-MariaDB", false},
		{"protocol 9", greetingPacket(0x09, "11.8.9", true), false, "11.8", "", false},
		{"error packet", greetingPacket(0xff, "Host blocked", true), false, "11.8", "", false},
		{"too long", big, false, "11.8", "", false},
		{"no NUL", greetingPacket(0x0a, "11.8.9", false)[:4+1+6], true, "11.8", "", false},
		{"length zero", []byte{0, 0, 0, 0}, true, "11.8", "", false},
		{"sequence 1", append([]byte{3, 0, 0, 1}, 0x0a, '1', 0), true, "11.8", "", false},
		{"control character", greetingPacket(0x0a, "11.8\n9", true), false, "11.8", "", false},
		{"EOF", nil, true, "11.8", "", false},
		{"short packet", []byte{50, 0, 0, 0, 0x0a, '1'}, true, "11.8", "", false},
		{"timeout", nil, false, "11.8", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := greetingConn(tt.data, tt.close)
			defer conn.Close()
			start := time.Now()
			v, err := readGreeting(conn)
			if time.Since(start) > greetingTimeout+time.Second {
				t.Errorf("readGreeting took %s", time.Since(start))
			}
			if tt.version == "" {
				if err == nil {
					t.Fatalf("readGreeting = %q, want an error", v)
				}
				return
			}
			if err != nil || v != tt.version {
				t.Fatalf("readGreeting = %q, %v; want %q", v, err, tt.version)
			}
			if got := versionMatches(v, tt.tag); got != tt.match {
				t.Errorf("versionMatches(%q, %q) = %v, want %v", v, tt.tag, got, tt.match)
			}
		})
	}
}
