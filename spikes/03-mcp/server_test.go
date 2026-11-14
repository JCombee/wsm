package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	idA    = "aaaaaaaaaaaa"
	idB    = "bbbbbbbbbbbb"
	tokenA = "tokA-3q2+7wQhKpL0Zx9mV4nB8cR1sT6uY5eW2aD0fG3h" // test-only values
	tokenB = "tokB-Jk8lMn2Op4Qr6St8Uv0Wx2Yz4Ab6Cd8Ef0Gh2Ij4k"
)

func testTokens(t *testing.T) []tokenEntry {
	t.Helper()
	tokens, err := parseTokens(Config{Sandboxes: []SandboxToken{
		{ID: idA, TokenSHA256: tokenHash(tokenA)},
		{ID: idB, TokenSHA256: tokenHash(tokenB)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

// memAudit collects records in memory and keeps the raw JSONL.
type memAudit struct {
	mu   sync.Mutex
	recs []Record
	raw  bytes.Buffer
}

func (m *memAudit) Write(r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
	return json.NewEncoder(&m.raw).Encode(r)
}

func (m *memAudit) records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.recs...)
}

func (m *memAudit) rawString() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.raw.String()
}

func newTestServer(t *testing.T, stateless bool, audit AuditWriter) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newHandler(testTokens(t), serverOptions{Stateless: stateless, Audit: audit, Logf: t.Logf}))
	t.Cleanup(srv.Close)
	return srv
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

// postInit sends a raw initialize request with the given Authorization
// header ("" for none).
func postInit(t *testing.T, url, authz string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// bearerTransport adds a bearer token to every request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func resultText(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

func TestBearerAuth(t *testing.T) {
	audit := &memAudit{}
	srv := newTestServer(t, false, audit)
	tests := []struct {
		name  string
		authz string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "Bearer not-a-real-token", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + tokenA, http.StatusUnauthorized},
		{"token without scheme", tokenA, http.StatusUnauthorized},
		{"good token", "Bearer " + tokenA, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := len(audit.records())
			resp := postInit(t, srv.URL+"/mcp/"+idA, tt.authz)
			if resp.StatusCode != tt.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.want)
			}
			recs := audit.records()
			if len(recs) != n+1 {
				t.Fatalf("%d new audit records, want 1", len(recs)-n)
			}
			r := recs[n]
			wantDecision, wantSandbox := "deny", ""
			if tt.want == http.StatusOK {
				wantDecision, wantSandbox = "allow", idA
			}
			if r.Status != tt.want || r.Decision != wantDecision || r.Sandbox != wantSandbox || r.RPCMethod != "initialize" {
				t.Errorf("audit record %+v", r)
			}
		})
	}
}

func TestPathMustMatchToken(t *testing.T) {
	audit := &memAudit{}
	srv := newTestServer(t, false, audit)
	for _, path := range []string{"/mcp/" + idB, "/mcp/cccccccccccc", "/mcp/" + idA + "x"} {
		resp := postInit(t, srv.URL+path, "Bearer "+tokenA)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("token of %s on %s: status %d, want 403", idA, path, resp.StatusCode)
		}
	}
	r := audit.records()[0]
	if r.Sandbox != idA || r.Path != "/mcp/"+idB || r.Decision != "deny" || r.Status != http.StatusForbidden {
		t.Errorf("audit record %+v", r)
	}
	// Other paths are not served at all, and still audited.
	resp := postInit(t, srv.URL+"/other", "Bearer "+tokenA)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/other: status %d, want 404", resp.StatusCode)
	}
	if n := len(audit.records()); n != 4 {
		t.Errorf("%d audit records, want 4", n)
	}
}

func TestToolsHaveNoIdentityArguments(t *testing.T) {
	srv := newTestServer(t, false, &memAudit{})
	cs := connect(t, srv.URL+"/mcp/"+idA, tokenA)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		b, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatal(err)
		}
		for prop := range schema.Properties {
			p := strings.ToLower(prop)
			if strings.Contains(p, "project") || strings.Contains(p, "sandbox") {
				t.Errorf("tool %s has identity argument %q", tool.Name, prop)
			}
		}
	}
	if got := strings.Join(names, ","); got != "sandbox_info,service_enable,services_available" {
		t.Errorf("tools %s", got)
	}
}

func TestServiceEnableDenied(t *testing.T) {
	audit := &memAudit{}
	srv := newTestServer(t, false, audit)
	cs := connect(t, srv.URL+"/mcp/"+idA, tokenA)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "service_enable", Arguments: map[string]any{"service": "mariadb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "not available in this spike") {
		t.Errorf("result isError=%v %q", res.IsError, resultText(res))
	}
	var found bool
	for _, r := range audit.records() {
		if r.RPCMethod == "tools/call" && r.Tool == "service_enable" {
			found = true
			if r.Decision != "deny" || r.Sandbox != idA || r.Reason == "" {
				t.Errorf("audit record %+v", r)
			}
		}
	}
	if !found {
		t.Error("no audit record for the service_enable call")
	}
}

// gateAudit blocks every write until release is closed, then returns err.
type gateAudit struct {
	entered chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (g *gateAudit) Write(Record) error {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.err
}

func TestAuditBeforeResponse(t *testing.T) {
	tests := []struct {
		name     string
		auditErr error
		want     int
	}{
		{"audit ok", nil, http.StatusOK},
		{"audit fails", errors.New("disk full"), http.StatusInternalServerError},
		{"audit fails on deny", errors.New("disk full"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &gateAudit{entered: make(chan struct{}), release: make(chan struct{}), err: tt.auditErr}
			srv := newTestServer(t, false, g)
			authz := "Bearer " + tokenA
			if strings.HasSuffix(tt.name, "on deny") {
				authz = ""
			}
			got := make(chan int, 1)
			go func() {
				req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/"+idA, strings.NewReader(initBody))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				if authz != "" {
					req.Header.Set("Authorization", authz)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					got <- -1
					return
				}
				resp.Body.Close()
				got <- resp.StatusCode
			}()
			select {
			case <-g.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("audit writer never called")
			}
			select {
			case code := <-got:
				t.Fatalf("response %d arrived before the audit write finished", code)
			case <-time.After(200 * time.Millisecond):
			}
			close(g.release)
			select {
			case code := <-got:
				if code != tt.want {
					t.Errorf("status %d, want %d", code, tt.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no response after release")
			}
		})
	}
}

func TestAuditRedactsTokens(t *testing.T) {
	audit := &memAudit{}
	srv := newTestServer(t, false, audit)
	postInit(t, srv.URL+"/mcp/"+idA, "Bearer "+tokenB+"x")
	postInit(t, srv.URL+"/mcp/"+idB, "Bearer "+tokenA)
	cs := connect(t, srv.URL+"/mcp/"+idA, tokenA)
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "sandbox_info"}); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	raw := audit.rawString()
	if raw == "" {
		t.Fatal("empty audit log")
	}
	for _, secret := range []string{tokenA, tokenB, "Bearer", "Authorization", "authorization"} {
		if strings.Contains(raw, secret) {
			t.Errorf("audit log contains %q", secret)
		}
	}
	// And no record carries a session ID.
	if strings.Contains(strings.ToLower(raw), "mcp-session-id") {
		t.Error("audit log contains a session header")
	}
}

func TestStatelessAndStateful(t *testing.T) {
	for _, stateless := range []bool{false, true} {
		name := "stateful"
		if stateless {
			name = "stateless"
		}
		t.Run(name, func(t *testing.T) {
			audit := &memAudit{}
			srv := newTestServer(t, stateless, audit)
			for _, c := range []struct{ id, token string }{{idA, tokenA}, {idB, tokenB}} {
				cs := connect(t, srv.URL+"/mcp/"+c.id, c.token)
				ctx := context.Background()
				tools, err := cs.ListTools(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(tools.Tools) != 3 {
					t.Errorf("%d tools, want 3", len(tools.Tools))
				}
				res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "sandbox_info"})
				if err != nil {
					t.Fatal(err)
				}
				if got := resultText(res); res.IsError || got != "sandbox "+c.id {
					t.Errorf("sandbox_info = %q (isError %v), want %q", got, res.IsError, "sandbox "+c.id)
				}
				cs.Close()
			}
			var calls, sessions int
			for _, r := range audit.records() {
				if r.RPCMethod == "tools/call" && r.Tool == "sandbox_info" && r.Decision == "allow" {
					calls++
				}
				if r.Session {
					sessions++
				}
			}
			if calls != 2 {
				t.Errorf("%d audited sandbox_info calls, want 2", calls)
			}
			if stateless && sessions != 0 {
				t.Errorf("stateless: %d requests carried a session", sessions)
			}
		})
	}
}
