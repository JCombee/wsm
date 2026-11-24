package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAdmin struct {
	mu        sync.Mutex
	projects  map[string]Project
	sandboxes []Sandbox
	created   []string
	actions   []string
}

func (f *fakeAdmin) Project(_ context.Context, slug string) (Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[slug]
	if !ok {
		return Project{}, fmt.Errorf("project %s not found", slug)
	}
	return p, nil
}

func (f *fakeAdmin) Sandboxes(_ context.Context, project string) ([]Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Sandbox
	for _, sb := range f.sandboxes {
		if sb.Project == project {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (f *fakeAdmin) CreateSandbox(_ context.Context, project, slug string) (Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := Sandbox{Project: project, Slug: slug, Status: "running", Pod: "wsm-" + project + "-" + slug, CreatedAt: time.Now()}
	f.sandboxes = append(f.sandboxes, sb)
	f.created = append(f.created, slug)
	return sb, nil
}

func (f *fakeAdmin) SandboxAction(_ context.Context, project, slug, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, project+"/"+slug+"/"+action)
	return nil
}

func (f *fakeAdmin) DeleteSandbox(_ context.Context, project, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, sb := range f.sandboxes {
		if sb.Project == project && sb.Slug == slug {
			f.sandboxes = append(f.sandboxes[:i], f.sandboxes[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("sandbox %s not found", slug)
}

const (
	uidA = 61001
	uidB = 61002
)

type testEnv struct {
	s         *Server
	admin     *fakeAdmin
	auditPath string
	a, b      *tenantHandler
	n         int
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ap := newAuditFile(t)
	aud, err := OpenAudit(ap)
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAdmin{projects: map[string]Project{
		"s06a": {Slug: "s06a", LinuxUser: "wsm-p-1", Image: "localhost/wsm-s06a:1", CPUMillis: 2000, MemBytes: 2 << 30, PidsLimit: 1024},
		"s06b": {Slug: "s06b", LinuxUser: "wsm-p-2", Image: "localhost/wsm-s06b:1", CPUMillis: 2000, MemBytes: 2 << 30, PidsLimit: 1024},
	}}
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, aud, fa, log.New(io.Discard, "", 0))
	return &testEnv{
		s: s, admin: fa, auditPath: ap,
		a: &tenantHandler{s: s, t: cfg.Tenants[0]},
		b: &tenantHandler{s: s, t: cfg.Tenants[1]},
	}
}

func (e *testEnv) call(h http.Handler, uid uint32, method, path, body string) *httptest.ResponseRecorder {
	e.n++
	req := httptest.NewRequest(method, "http://tenant"+path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, peerResult{peer: Peer{UID: uid, PID: 4242}}))
	req.Header.Set("X-S06-Req", fmt.Sprintf("req-%d", e.n))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func (e *testEnv) auditLines(t *testing.T) []Record {
	t.Helper()
	data, err := os.ReadFile(e.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAudit(bytes.NewReader(data)); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	var out []Record
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *testEnv) create(t *testing.T, h *tenantHandler, uid uint32) string {
	t.Helper()
	rr := e.call(h, uid, http.MethodPost, "/v1/tenant/sandboxes", `{}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rr.Code, rr.Body)
	}
	var sb TenantSandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	return sb.ID
}

func TestPeerMustBeTheControlRoom(t *testing.T) {
	e := newTestEnv(t)
	for _, uid := range []uint32{uidB, 0, 61001*65536 + 999} {
		rr := e.call(e.a, uid, http.MethodGet, "/v1/tenant/whoami", "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("uid %d: whoami = %d, want 403", uid, rr.Code)
		}
	}
	rr := e.call(e.a, uidA, http.MethodGet, "/v1/tenant/whoami", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"project":"s06a"`) {
		t.Fatalf("whoami = %d %s", rr.Code, rr.Body)
	}
	lines := e.auditLines(t)
	if len(lines) != 4 {
		t.Fatalf("audit has %d lines, want 4", len(lines))
	}
	if lines[0].Decision != "deny" || lines[0].PeerUID != uidB || lines[3].Decision != "allow" {
		t.Fatalf("audit lines = %+v", lines)
	}
}

func TestCreateCeiling(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"other image", `{"image":"docker.io/library/alpine"}`, http.StatusForbidden},
		{"project image", `{"image":"localhost/wsm-s06a:1"}`, http.StatusCreated},
		{"cpu above", `{"cpu_millis":2001}`, http.StatusForbidden},
		{"memory above", `{"mem_bytes":3221225472}`, http.StatusForbidden},
		{"pids above", `{"pids_limit":1025}`, http.StatusForbidden},
		{"negative", `{"pids_limit":-1}`, http.StatusBadRequest},
		{"unknown field", `{"privileged":true}`, http.StatusBadRequest},
		{"not json", `x`, http.StatusBadRequest},
		{"within", `{"cpu_millis":1000,"mem_bytes":1073741824,"pids_limit":512}`, http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			rr := e.call(e.a, uidA, http.MethodPost, "/v1/tenant/sandboxes", tt.body)
			if rr.Code != tt.want {
				t.Fatalf("create = %d %s, want %d", rr.Code, rr.Body, tt.want)
			}
			wantCreated := 0
			if tt.want == http.StatusCreated {
				wantCreated = 1
			}
			if len(e.admin.created) != wantCreated {
				t.Fatalf("created %d sandboxes, want %d", len(e.admin.created), wantCreated)
			}
		})
	}
}

func TestCreateCountsTheCeiling(t *testing.T) {
	e := newTestEnv(t)
	first := e.create(t, e.a, uidA)
	e.create(t, e.a, uidA)
	if rr := e.call(e.a, uidA, http.MethodPost, "/v1/tenant/sandboxes", `{}`); rr.Code != http.StatusForbidden {
		t.Fatalf("third create = %d, want 403", rr.Code)
	}
	if rr := e.call(e.a, uidA, http.MethodDelete, "/v1/tenant/sandboxes/"+first, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rr.Code, rr.Body)
	}
	e.create(t, e.a, uidA)
}

func TestOtherTenantSandboxIsNotFound(t *testing.T) {
	e := newTestEnv(t)
	id := e.create(t, e.a, uidA)
	random := "tzzzzzzzzzz"
	for _, tc := range []struct{ method, suffix string }{
		{http.MethodGet, ""}, {http.MethodDelete, ""}, {http.MethodPost, "/start"}, {http.MethodPost, "/stop"},
	} {
		got := e.call(e.b, uidB, tc.method, "/v1/tenant/sandboxes/"+id+tc.suffix, "")
		ref := e.call(e.b, uidB, tc.method, "/v1/tenant/sandboxes/"+random+tc.suffix, "")
		if got.Code != http.StatusNotFound || got.Body.String() != ref.Body.String() || got.Code != ref.Code {
			t.Fatalf("%s %s: got %d %q, random id gives %d %q", tc.method, tc.suffix, got.Code, got.Body, ref.Code, ref.Body)
		}
	}
	if len(e.admin.actions) != 0 {
		t.Fatalf("actions ran: %v", e.admin.actions)
	}
	rr := e.call(e.b, uidB, http.MethodGet, "/v1/tenant/sandboxes", "")
	if strings.Contains(rr.Body.String(), id) {
		t.Fatalf("B's list shows A's sandbox: %s", rr.Body)
	}
	// A still has its sandbox.
	if rr := e.call(e.a, uidA, http.MethodGet, "/v1/tenant/sandboxes/"+id, ""); rr.Code != http.StatusOK {
		t.Fatalf("A get = %d", rr.Code)
	}
}

func TestEveryRequestIsAuditedOnce(t *testing.T) {
	e := newTestEnv(t)
	id := e.create(t, e.a, uidA)
	calls := []struct {
		uid          uint32
		method, path string
		body         string
	}{
		{uidA, http.MethodGet, "/v1/tenant/sandboxes", ""},
		{uidA, http.MethodGet, "/v1/tenant/sandboxes/" + id, ""},
		{uidA, http.MethodPost, "/v1/tenant/sandboxes/" + id + "/stop", ""},
		{uidA, http.MethodPost, "/v1/tenant/sandboxes/" + id + "/start", ""},
		{uidA, http.MethodPut, "/v1/tenant/sandboxes/" + id, ""},
		{uidA, http.MethodGet, "/v1/tenant/nope", ""},
		{uidA, http.MethodGet, "/v1/projects", ""},
		{uidA, http.MethodPost, "/v1/tenant/sandboxes", `{"bad":1}`},
		{uidB, http.MethodGet, "/v1/tenant/sandboxes", ""},
		{uidA, http.MethodGet, "/v1/tenant/sandboxes/BAD", ""},
	}
	for _, c := range calls {
		e.call(e.a, c.uid, c.method, c.path, c.body)
	}
	lines := e.auditLines(t)
	if len(lines) != 1+len(calls) {
		t.Fatalf("audit has %d lines for %d calls", len(lines), 1+len(calls))
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if l.ReqID == "" || seen[l.ReqID] {
			t.Fatalf("missing or repeated request id in %+v", l)
		}
		seen[l.ReqID] = true
		if l.Sandbox != "" && !sandboxIDRE.MatchString(l.Sandbox) {
			t.Fatalf("audit holds a caller-chosen sandbox id %q", l.Sandbox)
		}
	}
}

func TestAuditFailureStopsTheCall(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file modes")
	}
	e := newTestEnv(t)
	if err := os.Chmod(e.auditPath, 0o400); err != nil {
		t.Fatal(err)
	}
	rr := e.call(e.a, uidA, http.MethodPost, "/v1/tenant/sandboxes", `{}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("create = %d, want 503", rr.Code)
	}
	if len(e.admin.created) != 0 {
		t.Fatalf("created %v while the audit was unwritable", e.admin.created)
	}
}

func TestParseRoute(t *testing.T) {
	tests := []struct {
		method, path string
		route, id    string
		status       int
	}{
		{"GET", "/v1/tenant/whoami", "whoami", "", 0},
		{"POST", "/v1/tenant/whoami", "whoami", "", 405},
		{"GET", "/v1/tenant/sandboxes", "list", "", 0},
		{"POST", "/v1/tenant/sandboxes", "create", "", 0},
		{"GET", "/v1/tenant/sandboxes/t0123456789", "get", "t0123456789", 0},
		{"DELETE", "/v1/tenant/sandboxes/t0123456789", "delete", "t0123456789", 0},
		{"POST", "/v1/tenant/sandboxes/t0123456789/exec", "exec", "t0123456789", 0},
		{"GET", "/v1/tenant/sandboxes/t0123456789/exec", "exec", "t0123456789", 405},
		{"POST", "/v1/tenant/sandboxes/t0123456789/kill", "unknown", "", 404},
		{"GET", "/v1/projects", "unknown", "", 404},
	}
	for _, tt := range tests {
		route, id, status := parseRoute(tt.method, tt.path)
		if route != tt.route || id != tt.id || status != tt.status {
			t.Errorf("parseRoute(%s %s) = %q %q %d, want %q %q %d", tt.method, tt.path, route, id, status, tt.route, tt.id, tt.status)
		}
	}
}
