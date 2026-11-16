package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JCombee/wsm/internal/audit"
	"github.com/JCombee/wsm/internal/broker"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/mcptoken"
)

type testSandbox struct {
	id, slug, token string
}

type testEnv struct {
	t     *testing.T
	store *db.Store
	audit *audit.Log
	srv   *Server
	ts    *httptest.Server
	a, b  testSandbox

	mu   sync.Mutex
	logs []string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	e := &testEnv{t: t, store: db.NewStore(sqlDB), audit: audit.New(sqlDB)}
	p := &db.Project{Slug: "proj-a", Name: "a", ImageRef: "img", CPUMillis: 1000, MemBytes: 1 << 30, PidsLimit: 100}
	if err := e.store.CreateProject(ctx, p, func(uid int) string { return fmt.Sprint("wsm-p-", uid) }); err != nil {
		t.Fatal(err)
	}
	q := &db.Project{Slug: "proj-b", Name: "b", ImageRef: "img", CPUMillis: 1000, MemBytes: 1 << 30, PidsLimit: 100}
	if err := e.store.CreateProject(ctx, q, func(uid int) string { return fmt.Sprint("wsm-p-", uid) }); err != nil {
		t.Fatal(err)
	}
	e.a = e.addSandbox(p.ID, "alpha", "agent/alpha")
	e.b = e.addSandbox(q.ID, "beta", "")
	e.srv = &Server{Store: e.store, Audit: e.audit, Logf: e.logf, Version: "test"}
	e.ts = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *testEnv) addSandbox(projectID, slug, branch string) testSandbox {
	e.t.Helper()
	token, hash, err := mcptoken.New()
	if err != nil {
		e.t.Fatal(err)
	}
	sb := &db.Sandbox{ProjectID: projectID, Slug: slug, Branch: branch, MCPTokenHash: hash}
	if err := e.store.CreateSandbox(context.Background(), sb, func(id string) string { return "sbx-" + id }); err != nil {
		e.t.Fatal(err)
	}
	return testSandbox{id: sb.ID, slug: slug, token: token}
}

func (e *testEnv) logf(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logs = append(e.logs, fmt.Sprintf(format, args...))
}

func (e *testEnv) url(sandboxID string) string { return e.ts.URL + "/mcp/" + sandboxID }

// lastSeq returns the highest audit sequence number.
func (e *testEnv) lastSeq() int64 {
	var n sql.NullInt64
	if err := e.store.DB.QueryRow(`SELECT max(seq) FROM audit_events`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n.Int64
}

// eventsAfter returns the audit events after seq.
func (e *testEnv) eventsAfter(seq int64) []audit.Record {
	rows, err := e.store.DB.Query(`SELECT seq, actor, coalesce(sandbox_id, ''), tool, args_json, result, detail
		FROM audit_events WHERE seq > ? ORDER BY seq`, seq)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []audit.Record
	for rows.Next() {
		var r audit.Record
		if err := rows.Scan(&r.Seq, &r.Actor, &r.SandboxID, &r.Tool, &r.ArgsJSON, &r.Result, &r.Detail); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// post sends one raw request and returns the status and body.
func (e *testEnv) post(url, authz, body string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const (
	initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	toolsListBody  = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
)

func callBody(tool, args string) string {
	return `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
}

// rpcResult extracts the JSON-RPC result from a JSON or SSE response.
func rpcResult(t *testing.T, body string) json.RawMessage {
	t.Helper()
	payload := body
	if i := strings.Index(body, "data: "); i >= 0 {
		payload = body[i+len("data: "):]
		if j := strings.IndexByte(payload, '\n'); j >= 0 {
			payload = payload[:j]
		}
	}
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		t.Fatalf("response is not JSON-RPC: %v: %q", err, body)
	}
	if msg.Result == nil {
		t.Fatalf("no result in response: %s", payload)
	}
	return msg.Result
}

type toolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func parseToolResult(t *testing.T, body string) toolResult {
	t.Helper()
	var r toolResult
	if err := json.Unmarshal(rpcResult(t, body), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r toolResult) text() string {
	var parts []string
	for _, c := range r.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, " ")
}

func bearer(token string) string { return "Bearer " + token }

func TestBearerTokenAndPath(t *testing.T) {
	e := newTestEnv(t)
	gone := e.addSandbox(e.mustProjectID("proj-a"), "gone", "")
	if err := e.store.DeleteSandbox(context.Background(), gone.id); err != nil {
		t.Fatal(err)
	}
	unauthorized := []struct {
		name, authz string
	}{
		{"no header", ""},
		{"basic", "Basic " + e.a.token},
		{"other scheme", "Token " + e.a.token},
		{"unknown token", bearer("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO-_")},
		{"malformed token", bearer("x")},
		{"deleted sandbox", bearer(gone.token)},
	}
	for _, tt := range unauthorized {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, e.url(e.a.id), strings.NewReader(initializeBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tt.authz != "" {
				req.Header.Set("Authorization", tt.authz)
			}
			resp, err := e.ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status %d, want 401", resp.StatusCode)
			}
			for _, h := range resp.Header.Values("WWW-Authenticate") {
				if strings.Contains(h, "resource_metadata") {
					t.Errorf("WWW-Authenticate has resource_metadata: %q", h)
				}
			}
		})
	}
	if code, _ := e.post(e.url(e.a.id), bearer(e.a.token), initializeBody); code != http.StatusOK {
		t.Errorf("token A on path A: status %d, want 200", code)
	}
	if code, _ := e.post(e.url(e.b.id), bearer(e.a.token), initializeBody); code != http.StatusForbidden {
		t.Errorf("token A on path B: status %d, want 403", code)
	}
	if code, _ := e.post(e.ts.URL+"/mcp", bearer(e.a.token), initializeBody); code != http.StatusNotFound {
		t.Errorf("token A on /mcp: status %d, want 404", code)
	}
	if code, _ := e.post(e.url(e.a.id)+"/x", bearer(e.a.token), initializeBody); code != http.StatusNotFound {
		t.Errorf("token A below path A: status %d, want 404", code)
	}
}

func (e *testEnv) mustProjectID(slug string) string {
	p, err := e.store.ProjectBySlug(context.Background(), slug)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.ID
}

// checkingWriter fails the test when a response byte or header goes out
// before the audit log grew.
type checkingWriter struct {
	*httptest.ResponseRecorder
	t      *testing.T
	e      *testEnv
	before int64
	seen   bool
}

func (w *checkingWriter) check() {
	if w.seen {
		return
	}
	w.seen = true
	if n := len(w.e.eventsAfter(w.before)); n != 1 {
		w.t.Errorf("response started with %d new audit events, want 1", n)
	}
}

func (w *checkingWriter) WriteHeader(code int) { w.check(); w.ResponseRecorder.WriteHeader(code) }
func (w *checkingWriter) Write(b []byte) (int, error) {
	w.check()
	return w.ResponseRecorder.Write(b)
}
func (w *checkingWriter) Flush() { w.check(); w.ResponseRecorder.Flush() }

type failingAuditor struct{}

func (failingAuditor) Append(context.Context, audit.Event) (audit.Record, error) {
	return audit.Record{}, errors.New("disk full")
}

func TestAuditBeforeResponse(t *testing.T) {
	e := newTestEnv(t)
	h := e.srv.Handler()
	requests := []struct {
		name, path, authz, body string
	}{
		{"unauthorized", "/mcp/" + e.a.id, "", initializeBody},
		{"forbidden", "/mcp/" + e.b.id, bearer(e.a.token), initializeBody},
		{"initialize", "/mcp/" + e.a.id, bearer(e.a.token), initializeBody},
		{"tools/list", "/mcp/" + e.a.id, bearer(e.a.token), toolsListBody},
		{"sandbox_info", "/mcp/" + e.a.id, bearer(e.a.token), callBody("sandbox_info", "{}")},
		{"service_enable", "/mcp/" + e.a.id, bearer(e.a.token), callBody("service_enable", `{"block":"mariadb"}`)},
		{"batch", "/mcp/" + e.a.id, bearer(e.a.token), "[" + toolsListBody + "]"},
	}
	for _, tt := range requests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tt.authz != "" {
				req.Header.Set("Authorization", tt.authz)
			}
			w := &checkingWriter{ResponseRecorder: httptest.NewRecorder(), t: t, e: e, before: e.lastSeq()}
			h.ServeHTTP(w, req)
			if !w.seen {
				t.Error("handler wrote no response")
			}
		})
	}

	t.Run("failing append", func(t *testing.T) {
		srv := &Server{Store: e.store, Audit: failingAuditor{}, Logf: e.logf}
		for _, body := range []string{callBody("sandbox_info", "{}"), toolsListBody} {
			req := httptest.NewRequest(http.MethodPost, "/mcp/"+e.a.id, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", bearer(e.a.token))
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("status %d, want 500", w.Code)
			}
			if got := w.Body.String(); strings.Contains(got, "proj-a") || strings.Contains(got, "sandbox_info") ||
				strings.Contains(got, "jsonrpc") {
				t.Errorf("body holds tool output: %q", got)
			}
		}
	})
}

func TestEveryRequestAudited(t *testing.T) {
	e := newTestEnv(t)
	big := callBody("sandbox_info", `{"x":"`+strings.Repeat("a", MaxBodyBytes)+`"}`)
	tests := []struct {
		name, url, authz, body string
		status                 int
		actor, sandbox, tool   string
		result                 string
	}{
		{"401", e.url(e.a.id), "", initializeBody, 401, "mcp:unauthenticated", "", "http:POST", "denied"},
		{"401 bad token", e.url(e.a.id), bearer("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO-_"), initializeBody, 401, "mcp:unauthenticated", "", "http:POST", "denied"},
		{"403", e.url(e.b.id), bearer(e.a.token), initializeBody, 403, "mcp:" + e.a.id, e.a.id, "http:POST", "denied"},
		{"404", e.ts.URL + "/other", bearer(e.a.token), initializeBody, 404, "mcp:" + e.a.id, e.a.id, "http:POST", "denied"},
		{"413", e.url(e.a.id), bearer(e.a.token), big, 413, "mcp:" + e.a.id, e.a.id, "http:POST", "denied"},
		{"400 batch", e.url(e.a.id), bearer(e.a.token), "[" + toolsListBody + "]", 400, "mcp:" + e.a.id, e.a.id, "http:POST", "denied"},
		{"400 duplicate key", e.url(e.a.id), bearer(e.a.token), `{"jsonrpc":"2.0","id":1,"method":"tools/list","method":"x"}`, 400, "mcp:" + e.a.id, e.a.id, "http:POST", "denied"},
		{"initialize", e.url(e.a.id), bearer(e.a.token), initializeBody, 200, "mcp:" + e.a.id, e.a.id, "rpc:initialize", "ok"},
		{"tools/list", e.url(e.a.id), bearer(e.a.token), toolsListBody, 200, "mcp:" + e.a.id, e.a.id, "rpc:tools/list", "ok"},
		{"sandbox_info", e.url(e.a.id), bearer(e.a.token), callBody("sandbox_info", `{"token":"`+e.b.token+`"}`), 200, "mcp:" + e.a.id, e.a.id, "sandbox_info", "ok"},
		{"service_enable", e.url(e.a.id), bearer(e.a.token), callBody("service_enable", `{"block":"mariadb"}`), 200, "mcp:" + e.a.id, e.a.id, "service_enable", "denied"},
		{"unknown tool", e.url(e.a.id), bearer(e.a.token), callBody("drop_all", `{}`), 200, "mcp:" + e.a.id, e.a.id, "drop_all", "denied"},
		{"odd tool name", e.url(e.a.id), bearer(e.a.token), callBody("Drop All", `{}`), 200, "mcp:" + e.a.id, e.a.id, "invalid", "denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := e.lastSeq()
			code, _ := e.post(tt.url, tt.authz, tt.body)
			if code != tt.status {
				t.Errorf("status %d, want %d", code, tt.status)
			}
			evs := e.eventsAfter(before)
			if len(evs) != 1 {
				t.Fatalf("%d events, want 1", len(evs))
			}
			ev := evs[0]
			if ev.Actor != tt.actor || ev.SandboxID != tt.sandbox || ev.Tool != tt.tool || ev.Result != tt.result {
				t.Errorf("event = actor %q sandbox %q tool %q result %q, want %q %q %q %q",
					ev.Actor, ev.SandboxID, ev.Tool, ev.Result, tt.actor, tt.sandbox, tt.tool, tt.result)
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(ev.ArgsJSON), &args); err != nil {
				t.Fatal(err)
			}
			if args["status"] != float64(tt.status) || args["http_method"] != "POST" {
				t.Errorf("args = %s", ev.ArgsJSON)
			}
			if tt.authz != "" && args["auth_scheme"] != "bearer" {
				t.Errorf("auth_scheme = %v", args["auth_scheme"])
			}
		})
	}

	// Large tool arguments are summarised, never stored whole.
	before := e.lastSeq()
	e.post(e.url(e.a.id), bearer(e.a.token), callBody("sandbox_info", `{"x":"`+strings.Repeat("a", 20<<10)+`"}`))
	evs := e.eventsAfter(before)
	if len(evs) != 1 || !strings.Contains(evs[0].ArgsJSON, `"truncated":true`) || len(evs[0].ArgsJSON) > 2048 {
		t.Errorf("large arguments: %d events, args %.200s", len(evs), evs[0].ArgsJSON)
	}

	// No event and no log line holds a token.
	all := e.eventsAfter(0)
	for _, ev := range all {
		line := ev.Actor + ev.Tool + ev.ArgsJSON + ev.Detail
		if strings.Contains(line, e.a.token) || strings.Contains(line, e.b.token) {
			t.Errorf("event %d holds a token", ev.Seq)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.logs {
		if strings.Contains(l, e.a.token) || strings.Contains(l, e.b.token) {
			t.Error("a log line holds a token")
		}
	}
	if n, err := e.audit.Verify(context.Background()); err != nil || n != int64(len(all)) {
		t.Errorf("Verify = %d, %v", n, err)
	}
}

func TestRejectsAmbiguousRequests(t *testing.T) {
	e := newTestEnv(t)
	tests := []struct{ name, body string }{
		{"batch", "[" + callBody("sandbox_info", "{}") + "]"},
		{"batch with space", " \n[" + toolsListBody + "]"},
		{"duplicate at top", `{"jsonrpc":"2.0","id":1,"method":"tools/call","method":"tools/list","params":{"name":"sandbox_info"}}`},
		{"case at top", `{"jsonrpc":"2.0","id":1,"method":"tools/list","Method":"tools/call","params":{"name":"service_enable"}}`},
		{"duplicate in params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sandbox_info","name":"service_enable"}}`},
		{"case in params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sandbox_info","NAME":"service_enable"}}`},
		{"duplicate in arguments", callBody("sandbox_info", `{"a":1,"a":2}`)},
		{"case in arguments", callBody("sandbox_info", `{"block":"x","Block":"y"}`)},
		{"kelvin sign in arguments", callBody("sandbox_info", `{"k":"x","K":"y"}`)},
		{"deep duplicate", callBody("sandbox_info", `{"a":[{"b":{"c":1,"C":2}}]}`)},
		{"not an object", `"tools/list"`},
		{"trailing data", toolsListBody + toolsListBody},
		{"invalid json", `{"jsonrpc":"2.0",`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := e.lastSeq()
			code, body := e.post(e.url(e.a.id), bearer(e.a.token), tt.body)
			if code != http.StatusBadRequest {
				t.Errorf("status %d, want 400: %s", code, body)
			}
			evs := e.eventsAfter(before)
			if len(evs) != 1 || evs[0].Result != audit.ResultDenied {
				t.Errorf("events = %+v, want one denied", evs)
			}
		})
	}
}

// connect opens an SDK client session for a sandbox.
func (e *testEnv) connect(sb testSandbox) *sdk.ClientSession {
	e.t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   e.url(sb.id),
		HTTPClient: &http.Client{Transport: bearerTransport{token: sb.token, base: e.ts.Client().Transport}},
	}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cs.Close() })
	return cs
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", bearer(b.token))
	return b.base.RoundTrip(r)
}

var identityName = regexp.MustCompile(`(?i)project|sandbox|(^|_)id$|^id`)

func TestToolsTakeNoIdentity(t *testing.T) {
	e := newTestEnv(t)
	cs := e.connect(e.a)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		b, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil || schema.Type != "object" {
			t.Errorf("%s: schema %s", tool.Name, b)
		}
		for p := range schema.Properties {
			if identityName.MatchString(p) {
				t.Errorf("%s has an identity-like input %q", tool.Name, p)
			}
		}
		readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		switch tool.Name {
		case "sandbox_info", "services_available":
			if len(schema.Properties) != 0 {
				t.Errorf("%s has input properties: %s", tool.Name, b)
			}
			if !readOnly {
				t.Errorf("%s lacks readOnlyHint", tool.Name)
			}
		case "service_enable":
			if readOnly {
				t.Error("service_enable has readOnlyHint")
			}
			if len(schema.Properties) != 2 || schema.Properties["block"] == nil || schema.Properties["tag"] == nil {
				t.Errorf("service_enable input: %s", b)
			}
		}
	}
	want := map[string]bool{"sandbox_info": true, "services_available": true, "service_enable": true}
	for _, n := range names {
		delete(want, n)
	}
	if len(names) != 3 || len(want) != 0 {
		t.Errorf("tools = %v, want exactly the three", names)
	}
}

func TestSandboxInfoUsesToken(t *testing.T) {
	e := newTestEnv(t)
	cs := e.connect(e.a)
	for _, args := range []map[string]any{
		nil,
		{"sandbox": e.b.slug, "project": "proj-b", "sandbox_id": e.b.id, "id": e.b.id, "token": e.b.token},
	} {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "sandbox_info", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("sandbox_info failed: %v", res.Content)
		}
		b, _ := json.Marshal(res.StructuredContent)
		var info SandboxInfo
		if err := json.Unmarshal(b, &info); err != nil {
			t.Fatal(err)
		}
		if info.Project != "proj-a" || info.Sandbox != "alpha" || info.Branch != "agent/alpha" ||
			info.PreviewURL != "" || info.Services == nil || len(info.Services) != 0 {
			t.Errorf("sandbox_info = %+v", info)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		if strings.Contains(text, "proj-b") || strings.Contains(text, e.b.id) || strings.Contains(text, "beta") {
			t.Errorf("sandbox_info names sandbox B: %s", text)
		}
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "services_available"})
	if err != nil || res.IsError {
		t.Fatalf("services_available: %v %v", res, err)
	}
	if b, _ := json.Marshal(res.StructuredContent); string(b) != `{"services":[]}` {
		t.Errorf("services_available = %s", b)
	}

	// last_activity_at follows authenticated requests on the own path.
	_, sb, err := e.store.SandboxByTokenHash(context.Background(), mcptoken.Hash(e.a.token))
	if err != nil || sb.LastActivityAt == nil {
		t.Errorf("last_activity_at of A not set: %+v %v", sb, err)
	}
	_, sbB, _ := e.store.SandboxByTokenHash(context.Background(), mcptoken.Hash(e.b.token))
	if sbB.LastActivityAt != nil {
		t.Error("last_activity_at of B set by A's requests")
	}
}

// fakeServices is a broker with one ceiling: mariadb on 11.8 and 12.3 for
// the sandbox in allowed. Block "slow" is unavailable, "boom" fails.
type fakeServices struct {
	mu      sync.Mutex
	allowed string
	calls   []string // sandbox block tag, per Enable
	grants  map[string][]broker.Credentials
}

const fakePassword = "SECRETpw0123456789abcdefghijklmnopqrstuvwxy"

func (f *fakeServices) Enable(_ context.Context, sandboxID, block, tag string) (broker.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sandboxID+" "+block+" "+tag)
	switch block {
	case "slow":
		return broker.Credentials{}, fmt.Errorf("%w: slow 11.8", broker.ErrUnavailable)
	case "boom":
		return broker.Credentials{}, errors.New("database is locked")
	case "mariadb":
	default:
		return broker.Credentials{}, fmt.Errorf("%w: unknown block %q", broker.ErrDenied, block)
	}
	if sandboxID != f.allowed {
		return broker.Credentials{}, fmt.Errorf("%w: block mariadb is not allowed for this project", broker.ErrDenied)
	}
	if tag == "" {
		tag = "11.8"
	}
	if tag != "11.8" && tag != "12.3" {
		return broker.Credentials{}, fmt.Errorf("%w: tag %s of block mariadb is not allowed for this project", broker.ErrDenied, tag)
	}
	c := broker.Credentials{Block: block, Tag: tag, Host: "10.255.0.1", Port: 20000, User: "sbx_" + sandboxID,
		Password: fakePassword, Database: "sbx_" + sandboxID + "_app"}
	if tag == "12.3" {
		c.Port = 20001
	}
	for _, g := range f.grants[sandboxID] {
		if g.Tag == tag {
			return g, nil
		}
	}
	if f.grants == nil {
		f.grants = map[string][]broker.Credentials{}
	}
	f.grants[sandboxID] = append(f.grants[sandboxID], c)
	return c, nil
}

func (f *fakeServices) Enabled(_ context.Context, sandboxID string) ([]broker.Enabled, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []broker.Enabled{}
	for _, g := range f.grants[sandboxID] {
		out = append(out, broker.Enabled{Block: g.Block, Tag: g.Tag, Host: g.Host, Port: g.Port, User: g.User, Database: g.Database})
	}
	return out, nil
}

func (f *fakeServices) Available(_ context.Context, sandboxID string) ([]broker.Offer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sandboxID != f.allowed {
		return nil, nil
	}
	o := broker.Offer{Block: "mariadb", Profile: "dev", Tags: []string{"11.8", "12.3"}, DefaultTag: "11.8"}
	for _, g := range f.grants[sandboxID] {
		o.EnabledTags = append(o.EnabledTags, g.Tag)
	}
	return []broker.Offer{o}, nil
}

// callEnable calls service_enable from sb with raw arguments and returns
// the tool result and the audit event of the call.
func (e *testEnv) callEnable(sb testSandbox, args string) (toolResult, audit.Record) {
	e.t.Helper()
	before := e.lastSeq()
	code, body := e.post(e.url(sb.id), bearer(sb.token), callBody("service_enable", args))
	if code != 200 {
		e.t.Fatalf("service_enable %s: status %d: %s", args, code, body)
	}
	tr := parseToolResult(e.t, body)
	evs := e.eventsAfter(before)
	if len(evs) != 1 || evs[0].Tool != "service_enable" {
		e.t.Fatalf("events for one call: %+v", evs)
	}
	return tr, evs[0]
}

func TestServiceEnableTool(t *testing.T) {
	e := newTestEnv(t)
	fake := &fakeServices{allowed: e.a.id}
	e.srv.Services = fake

	tr, ev := e.callEnable(e.a, `{"block":"mariadb"}`)
	if tr.IsError {
		t.Fatalf("service_enable failed: %s", tr.text())
	}
	var fields map[string]any
	if err := json.Unmarshal(tr.StructuredContent, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"block": "mariadb", "tag": "11.8", "host": "10.255.0.1", "port": float64(20000),
		"user": "sbx_" + e.a.id, "password": fakePassword, "database": "sbx_" + e.a.id + "_app"}
	if fmt.Sprint(fields) != fmt.Sprint(want) {
		t.Errorf("structured = %v, want %v", fields, want)
	}
	var text map[string]any
	if err := json.Unmarshal([]byte(tr.text()), &text); err != nil || fmt.Sprint(text) != fmt.Sprint(want) {
		t.Errorf("text = %s", tr.text())
	}
	if ev.Result != audit.ResultOK || ev.Detail != "granted mariadb 11.8" || ev.Actor != "mcp:"+e.a.id {
		t.Errorf("event = %+v", ev)
	}
	if strings.Contains(ev.ArgsJSON, fakePassword) || strings.Contains(ev.Detail, fakePassword) {
		t.Error("the audit event contains the password")
	}
	if fake.calls[0] != e.a.id+" mariadb " {
		t.Errorf("Enable got %q", fake.calls[0])
	}

	// With a tag.
	tr, ev = e.callEnable(e.a, `{"block":"mariadb","tag":"12.3"}`)
	if tr.IsError || !strings.Contains(tr.text(), `"port":20001`) || ev.Detail != "granted mariadb 12.3" {
		t.Errorf("12.3: %s, %+v", tr.text(), ev)
	}

	// Unavailable and internal errors.
	tr, ev = e.callEnable(e.a, `{"block":"slow"}`)
	if !tr.IsError || tr.text() != "error: service not available, try again later" || ev.Result != audit.ResultError {
		t.Errorf("unavailable: %q, %+v", tr.text(), ev)
	}
	tr, ev = e.callEnable(e.a, `{"block":"boom"}`)
	if !tr.IsError || tr.text() != "error: internal error" || ev.Result != audit.ResultError ||
		strings.Contains(tr.text(), "locked") {
		t.Errorf("internal: %q, %+v", tr.text(), ev)
	}

	// sandbox_info and services_available show the grants, no password.
	cs := e.connect(e.a)
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "sandbox_info"})
	if err != nil || res.IsError {
		t.Fatalf("sandbox_info: %v %v", res, err)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var info SandboxInfo
	if err := json.Unmarshal(b, &info); err != nil || len(info.Services) != 2 ||
		info.Services[0] != (ServiceInfo{Block: "mariadb", Tag: "11.8", Host: "10.255.0.1", Port: 20000,
			User: "sbx_" + e.a.id, Database: "sbx_" + e.a.id + "_app"}) || info.Services[1].Port != 20001 {
		t.Errorf("sandbox_info = %s", b)
	}
	res2, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "services_available"})
	if err != nil || res2.IsError {
		t.Fatalf("services_available: %v %v", res2, err)
	}
	b2, _ := json.Marshal(res2.StructuredContent)
	var avail ServicesAvailable
	if err := json.Unmarshal(b2, &avail); err != nil || !reflect.DeepEqual(avail, ServicesAvailable{Services: []ServiceOffer{{
		Block: "mariadb", Profile: "dev", Tags: []string{"11.8", "12.3"}, DefaultTag: "11.8",
		EnabledTags: []string{"11.8", "12.3"}}}}) {
		t.Errorf("services_available = %s", b2)
	}
	for _, out := range []string{string(b), string(b2), res.Content[0].(*sdk.TextContent).Text,
		res2.Content[0].(*sdk.TextContent).Text} {
		if strings.Contains(out, fakePassword) || strings.Contains(strings.ToLower(out), "password") {
			t.Errorf("output shows a password: %s", out)
		}
	}
	// B sees nothing of A.
	csB := e.connect(e.b)
	res3, err := csB.CallTool(context.Background(), &sdk.CallToolParams{Name: "services_available"})
	if b3, _ := json.Marshal(res3.StructuredContent); err != nil || string(b3) != `{"services":[]}` {
		t.Errorf("services_available for B = %s, %v", b3, err)
	}

	// No log line and no audit event has the password.
	e.mu.Lock()
	for _, l := range e.logs {
		if strings.Contains(l, fakePassword) {
			t.Errorf("log has the password: %s", l)
		}
	}
	e.mu.Unlock()
	for _, ev := range e.eventsAfter(0) {
		if strings.Contains(ev.ArgsJSON+ev.Detail, fakePassword) {
			t.Errorf("audit event has the password: %+v", ev)
		}
	}
}

func TestServiceEnableDenied(t *testing.T) {
	e := newTestEnv(t)
	fake := &fakeServices{allowed: e.a.id}
	e.srv.Services = fake

	for _, tt := range []struct {
		name string
		sb   testSandbox
		args string
	}{
		{"project without ceiling", e.b, `{"block":"mariadb"}`},
		{"tag outside ceiling", e.a, `{"block":"mariadb","tag":"11.4"}`},
		{"unknown block", e.a, `{"block":"nosuchblock"}`},
		{"extra project field", e.a, `{"block":"mariadb","project":"proj-b"}`},
		{"extra sandbox field", e.a, `{"block":"mariadb","sandbox":"` + e.b.id + `"}`},
		{"Block in capitals", e.a, `{"Block":"mariadb"}`},
		{"tag with quote", e.a, `{"block":"mariadb","tag":"11.8'"}`},
		{"tag with semicolon", e.a, `{"block":"mariadb","tag":"11.8;DROP"}`},
		{"tag latest", e.a, `{"block":"mariadb","tag":"latest"}`},
		{"empty tag", e.a, `{"block":"mariadb","tag":""}`},
		{"tag number", e.a, `{"block":"mariadb","tag":11.8}`},
		{"block with quote", e.a, `{"block":"maria'db"}`},
		{"block upper", e.a, `{"block":"MariaDB"}`},
		{"block empty", e.a, `{"block":""}`},
		{"no block", e.a, `{}`},
		{"null arguments", e.a, `null`},
		{"array arguments", e.a, `["mariadb"]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr, ev := e.callEnable(tt.sb, tt.args)
			if !tr.IsError || !strings.HasPrefix(tr.text(), "denied:") {
				t.Errorf("result = isError %v, %q", tr.IsError, tr.text())
			}
			if ev.Result != audit.ResultDenied || ev.Actor != "mcp:"+tt.sb.id {
				t.Errorf("event = %+v", ev)
			}
		})
	}
	// Only the first three reached the broker; the rest failed the strict
	// decoding.
	if len(fake.calls) != 3 {
		t.Errorf("broker calls = %q, want 3", fake.calls)
	}

	// Without services, the handler denies too.
	e.srv.Services = nil
	if tr, ev := e.callEnable(e.a, `{"block":"mariadb"}`); !strings.HasPrefix(tr.text(), "denied:") ||
		ev.Result != audit.ResultDenied {
		t.Errorf("without services: %q, %+v", tr.text(), ev)
	}
	// And the handler on its own, without identity or record.
	r, err := e.srv.serviceEnable(context.Background(), &sdk.CallToolRequest{})
	if err != nil || !r.IsError || !strings.HasPrefix(r.Content[0].(*sdk.TextContent).Text, "denied:") {
		t.Errorf("serviceEnable handler = %+v, %v", r, err)
	}
}

func TestServerLimits(t *testing.T) {
	srv := NewHTTPServer(http.NotFoundHandler(), nil)
	tests := []struct {
		name      string
		got, want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 10 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 30 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 10 * time.Minute},
		{"IdleTimeout", srv.IdleTimeout, 2 * time.Minute},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if srv.MaxHeaderBytes != 16<<10 {
		t.Errorf("MaxHeaderBytes = %d", srv.MaxHeaderBytes)
	}
	if srv.Handler == nil {
		t.Error("no handler")
	}
}

func TestFoldKey(t *testing.T) {
	tests := []struct {
		a, b string
	}{
		{"method", "METHOD"}, {"k", "K"}, {"straße", "STRASSE"}, {"name", "nаme"},
	}
	for _, tt := range tests {
		if got, want := foldKey(tt.a) == foldKey(tt.b), strings.EqualFold(tt.a, tt.b); got != want {
			t.Errorf("foldKey(%q) == foldKey(%q) is %v, EqualFold says %v", tt.a, tt.b, got, want)
		}
	}
}
