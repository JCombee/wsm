//go:build isolation

package isolation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/mcptoken"
	"github.com/JCombee/wsm/internal/sandbox"
)

// dbPath is the manager's database; the tests only read it.
const dbPath = "/var/lib/wsm/wsm.db"

// probeInSandbox is where the test binary lands in a workspace container.
const probeInSandbox = "/tmp/wsm-probe"

// mcpRequest tells the MCP probe what to do. It arrives on stdin, so a
// token never shows up in argv.
type mcpRequest struct {
	// Op is "raw" (one HTTP POST), "call" (tools/call through the SDK
	// client) or "list" (tools/list through the SDK client).
	Op string `json:"op"`
	// URL defaults to WSM_MCP_URL.
	URL string `json:"url,omitempty"`
	// Auth is "bearer" (the default), "none", "basic" or "random".
	Auth string `json:"auth,omitempty"`
	// Token defaults to WSM_MCP_TOKEN.
	Token     string         `json:"token,omitempty"`
	Body      string         `json:"body,omitempty"`
	Tool      string         `json:"tool,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// mcpResponse is what the probe reports. It never holds a token.
type mcpResponse struct {
	Status           int             `json:"status,omitempty"`
	ResourceMetadata bool            `json:"resource_metadata,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	Text             string          `json:"text,omitempty"`
	Structured       json.RawMessage `json:"structured,omitempty"`
	Tools            []probeTool     `json:"tools,omitempty"`
	Error            string          `json:"error,omitempty"`
}

type probeTool struct {
	Name       string   `json:"name"`
	ReadOnly   bool     `json:"read_only"`
	Properties []string `json:"properties"`
}

const probeInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"wsm-probe","version":"0"}}}`

// mcpProbe runs inside a workspace container: it reads one mcpRequest
// from stdin, talks to the MCP server and prints one mcpResponse.
func mcpProbe(stdin io.Reader, stdout io.Writer) int {
	var req mcpRequest
	resp := mcpResponse{}
	defer func() { json.NewEncoder(stdout).Encode(resp) }()
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&req); err != nil {
		resp.Error = "decode request: " + err.Error()
		return 1
	}
	if req.URL == "" {
		req.URL = os.Getenv("WSM_MCP_URL")
	}
	if req.Token == "" {
		req.Token = os.Getenv("WSM_MCP_TOKEN")
	}
	var authz string
	switch req.Auth {
	case "", "bearer":
		authz = "Bearer " + req.Token
	case "basic":
		authz = "Basic " + base64.StdEncoding.EncodeToString([]byte("agent:"+req.Token))
	case "random":
		b := make([]byte, 32)
		rand.Read(b)
		authz = "Bearer " + base64.RawURLEncoding.EncodeToString(b)
	case "none":
	default:
		resp.Error = "unknown auth " + req.Auth
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if req.Op == "raw" {
		body := req.Body
		if body == "" {
			body = probeInitialize
		}
		hr, _ := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, strings.NewReader(body))
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("Accept", "application/json, text/event-stream")
		if authz != "" {
			hr.Header.Set("Authorization", authz)
		}
		r, err := (&http.Client{Timeout: 30 * time.Second}).Do(hr)
		if err != nil {
			resp.Error = scrub(err.Error(), req.Token)
			return 1
		}
		io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
		resp.Status = r.StatusCode
		for _, h := range r.Header.Values("WWW-Authenticate") {
			if strings.Contains(h, "resource_metadata") {
				resp.ResourceMetadata = true
			}
		}
		return 0
	}

	client := sdk.NewClient(&sdk.Implementation{Name: "wsm-probe", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   req.URL,
		HTTPClient: &http.Client{Transport: probeAuth{authz: authz}, Timeout: 30 * time.Second},
	}, nil)
	if err != nil {
		resp.Error = scrub("connect: "+err.Error(), req.Token)
		return 1
	}
	defer cs.Close()
	switch req.Op {
	case "list":
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			resp.Error = scrub(err.Error(), req.Token)
			return 1
		}
		for _, t := range res.Tools {
			pt := probeTool{Name: t.Name, ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint, Properties: []string{}}
			b, _ := json.Marshal(t.InputSchema)
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			json.Unmarshal(b, &schema)
			for p := range schema.Properties {
				pt.Properties = append(pt.Properties, p)
			}
			sort.Strings(pt.Properties)
			resp.Tools = append(resp.Tools, pt)
		}
	case "call":
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: req.Tool, Arguments: req.Arguments})
		if err != nil {
			resp.Error = scrub(err.Error(), req.Token)
			return 1
		}
		resp.IsError = res.IsError
		var parts []string
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				parts = append(parts, tc.Text)
			}
		}
		resp.Text = scrub(strings.Join(parts, " "), req.Token)
		if res.StructuredContent != nil {
			resp.Structured, _ = json.Marshal(res.StructuredContent)
		}
	default:
		resp.Error = "unknown op " + req.Op
		return 1
	}
	return 0
}

// scrub removes a token from text the probe prints.
func scrub(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[token]")
}

type probeAuth struct{ authz string }

func (p probeAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if p.authz != "" {
		r.Header.Set("Authorization", p.authz)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// probeCopied remembers the containers that have the probe.
var probeCopied sync.Map

// installProbe copies this test binary into p's workspace container, over
// exec with stdin: no mounts.
func installProbe(ctx context.Context, t *testing.T, p project) {
	t.Helper()
	if _, ok := probeCopied.Load(p.container); ok {
		return
	}
	f, err := os.Open(s.probeBin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := asUserIn(ctx, p, f, "podman", "exec", "-i", p.container, "dd", "of="+probeInSandbox, "status=none"); err != nil {
		t.Fatalf("copy the probe into %s: %v", p.slug, err)
	}
	if _, err := inSandbox(ctx, p, "chmod", "0755", probeInSandbox); err != nil {
		t.Fatal(err)
	}
	probeCopied.Store(p.container, true)
}

// mcpFromSandbox runs the MCP probe in p's workspace container.
func mcpFromSandbox(ctx context.Context, t *testing.T, p project, req mcpRequest) mcpResponse {
	t.Helper()
	installProbe(ctx, t, p)
	in, _ := json.Marshal(req)
	out, err := asUserIn(ctx, p, bytes.NewReader(in), "podman", "exec", "-i",
		"-e", probeEnv+"=mcp", p.container, probeInSandbox)
	var resp mcpResponse
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if jerr := json.Unmarshal([]byte(lines[len(lines)-1]), &resp); jerr != nil {
		t.Fatalf("mcp probe in %s: %v (%v)", p.slug, jerr, err)
	}
	return resp
}

// containerEnv reads a workspace container's environment as the project
// user, the way that user can. Errors never carry the output, which holds
// the token.
func containerEnv(ctx context.Context, p project, container string) (map[string]string, error) {
	out, err := asUser(ctx, p, "podman", "container", "inspect", "--format", "{{json .Config.Env}}", container)
	if err != nil {
		return nil, fmt.Errorf("inspect %s failed", container)
	}
	var list []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
		return nil, fmt.Errorf("inspect %s: unreadable env", container)
	}
	env := map[string]string{}
	for _, kv := range list {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env, nil
}

func mcpToken(ctx context.Context, t *testing.T, p project) string {
	t.Helper()
	env, err := containerEnv(ctx, p, p.container)
	if err != nil {
		t.Fatal(err)
	}
	tok := env["WSM_MCP_TOKEN"]
	if len(tok) != mcptoken.Length {
		t.Fatalf("%s: WSM_MCP_TOKEN has %d characters, want %d", p.slug, len(tok), mcptoken.Length)
	}
	return tok
}

type auditEvent struct {
	Seq       int64
	Actor     string
	SandboxID string
	Tool      string
	Result    string
	Args      string
}

func openDB(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

// auditHead returns the highest audit sequence number.
func auditHead(ctx context.Context, t *testing.T) int64 {
	t.Helper()
	sqlDB := openDB(ctx, t)
	defer sqlDB.Close()
	var n sql.NullInt64
	if err := sqlDB.QueryRowContext(ctx, `SELECT max(seq) FROM audit_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n.Int64
}

// auditAfter returns the audit events after seq.
func auditAfter(ctx context.Context, t *testing.T, seq int64) []auditEvent {
	t.Helper()
	sqlDB := openDB(ctx, t)
	defer sqlDB.Close()
	rows, err := sqlDB.QueryContext(ctx, `SELECT seq, actor, coalesce(sandbox_id, ''), tool, result, args_json
		FROM audit_events WHERE seq > ? ORDER BY seq`, seq)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditEvent
	for rows.Next() {
		var e auditEvent
		if err := rows.Scan(&e.Seq, &e.Actor, &e.SandboxID, &e.Tool, &e.Result, &e.Args); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// findEvent returns the first event with tool and result, if any.
func findEvent(evs []auditEvent, tool, result string) (auditEvent, bool) {
	for _, e := range evs {
		if e.Tool == tool && e.Result == result {
			return e, true
		}
	}
	return auditEvent{}, false
}

func sandboxInfoOf(t *testing.T, r mcpResponse) (project, sandbox string) {
	t.Helper()
	if r.Error != "" || r.IsError {
		t.Fatalf("sandbox_info failed: %s %s", r.Error, r.Text)
	}
	var info struct {
		Project string `json:"project"`
		Sandbox string `json:"sandbox"`
	}
	if err := json.Unmarshal(r.Structured, &info); err != nil {
		t.Fatalf("sandbox_info output: %v: %s", err, r.Structured)
	}
	return info.Project, info.Sandbox
}

func TestMCPSandboxInfo(t *testing.T) {
	ctx := ctxT(t)
	env, err := containerEnv(ctx, s.a, s.a.container)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := env["WSM_MCP_URL"], "http://10.255.0.1:7443/mcp/"+s.a.sandboxID; got != want {
		t.Errorf("WSM_MCP_URL = %q, want %q", got, want)
	}
	if n := len(env["WSM_MCP_TOKEN"]); n != 43 {
		t.Errorf("WSM_MCP_TOKEN has %d characters, want 43", n)
	}
	installProbe(ctx, t, s.a)

	beforeCounter := counter(t, "mcp_allowed")
	before := auditHead(ctx, t)
	r := mcpFromSandbox(ctx, t, s.a, mcpRequest{Op: "call", Tool: "sandbox_info"})
	proj, sb := sandboxInfoOf(t, r)
	if proj != "iso-a" || sb != "main" {
		t.Errorf("sandbox_info = project %q sandbox %q, want iso-a main", proj, sb)
	}
	if after := counter(t, "mcp_allowed"); after <= beforeCounter {
		t.Errorf("counter mcp_allowed did not move (%d -> %d)", beforeCounter, after)
	}
	ev, ok := findEvent(auditAfter(ctx, t, before), "sandbox_info", "ok")
	if !ok {
		t.Fatal("no ok event for sandbox_info")
	}
	if ev.Actor != "mcp:"+s.a.sandboxID || ev.SandboxID != s.a.sandboxID {
		t.Errorf("event actor %q sandbox %q, want mcp:%s", ev.Actor, ev.SandboxID, s.a.sandboxID)
	}

	// The tools take no identity.
	r = mcpFromSandbox(ctx, t, s.a, mcpRequest{Op: "list"})
	if r.Error != "" || len(r.Tools) != 3 {
		t.Fatalf("tools/list: %s %+v", r.Error, r.Tools)
	}
	for _, tool := range r.Tools {
		for _, prop := range tool.Properties {
			l := strings.ToLower(prop)
			if strings.Contains(l, "project") || strings.Contains(l, "sandbox") || l == "id" || strings.HasSuffix(l, "_id") {
				t.Errorf("%s has an identity-like input %q", tool.Name, prop)
			}
		}
	}
}

func TestMCPTokenOfOtherSandbox(t *testing.T) {
	ctx := ctxT(t)
	before := auditHead(ctx, t)
	r := mcpFromSandbox(ctx, t, s.a, mcpRequest{Op: "raw", URL: mcptoken.URL(s.b.sandboxID)})
	if r.Status != http.StatusForbidden {
		t.Errorf("A's token on B's path: status %d (%s), want 403", r.Status, r.Error)
	}
	evs := auditAfter(ctx, t, before)
	if len(evs) != 1 || evs[0].Result != "denied" || evs[0].SandboxID != s.a.sandboxID ||
		evs[0].Actor != "mcp:"+s.a.sandboxID {
		t.Errorf("events = %+v, want one denied event for sandbox A", evs)
	}

	// Arguments that name B change nothing.
	r = mcpFromSandbox(ctx, t, s.a, mcpRequest{Op: "call", Tool: "sandbox_info", Arguments: map[string]any{
		"project": s.b.slug, "sandbox": "main", "sandbox_id": s.b.sandboxID, "id": s.b.sandboxID,
	}})
	if proj, _ := sandboxInfoOf(t, r); proj != "iso-a" || strings.Contains(string(r.Structured), s.b.sandboxID) {
		t.Errorf("sandbox_info with B's names returned %s", r.Structured)
	}

	// B's token is nowhere in A: not in the environment, /workspace or
	// /home/agent.
	tokB := mcpToken(ctx, t, s.b)
	envA, err := inSandbox(ctx, s.a, "env")
	if err != nil {
		t.Fatal("env in sandbox A failed")
	}
	if strings.Contains(envA, tokB) {
		t.Error("B's token is in A's environment")
	}
	out, err := asUserIn(ctx, s.a, strings.NewReader(tokB+"\n"), "podman", "exec", "-i", s.a.container,
		"grep", "-rlsF", "-f", "-", "/workspace", "/home/agent", "/tmp")
	if err == nil || strings.TrimSpace(out) != "" {
		t.Errorf("B's token found in A's files: %q", out)
	}
}

func TestMCPRejectsBadTokens(t *testing.T) {
	ctx := ctxT(t)
	installProbe(ctx, t, s.a)
	for _, auth := range []string{"none", "random", "basic"} {
		t.Run(auth, func(t *testing.T) {
			before := auditHead(ctx, t)
			r := mcpFromSandbox(ctx, t, s.a, mcpRequest{Op: "raw", Auth: auth})
			if r.Status != http.StatusUnauthorized || r.ResourceMetadata {
				t.Errorf("status %d resource_metadata %v (%s), want 401 without", r.Status, r.ResourceMetadata, r.Error)
			}
			evs := auditAfter(ctx, t, before)
			if len(evs) != 1 || evs[0].Result != "denied" || evs[0].Actor != "mcp:unauthenticated" || evs[0].SandboxID != "" {
				t.Errorf("events = %+v, want one denied unauthenticated event", evs)
			}
		})
	}
}

func TestMCPServiceEnableDenied(t *testing.T) {
	ctx := ctxT(t)
	before := auditHead(ctx, t)
	r := mcpFromSandbox(ctx, t, s.a, mcpRequest{Op: "call", Tool: "service_enable", Arguments: map[string]any{"block": "mariadb"}})
	if r.Error != "" || !r.IsError || !strings.HasPrefix(r.Text, "denied:") {
		t.Errorf("service_enable = error %q isError %v text %q", r.Error, r.IsError, r.Text)
	}
	if _, ok := findEvent(auditAfter(ctx, t, before), "service_enable", "denied"); !ok {
		t.Error("no denied event for service_enable")
	}
}

func TestMCPTokenStoredOnlyAsHash(t *testing.T) {
	ctx := ctxT(t)
	tok := mcpToken(ctx, t, s.a)
	// grep reads the token from stdin and prints only file names; it
	// exits 1 when nothing matches.
	// The search covers wsm.db-wal too. Make sure it exists, so the
	// check is not empty.
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Errorf("control: %s-wal: %v", dbPath, err)
	}
	out, err := runIn(ctx, strings.NewReader(tok+"\n"), "grep", "-rlsF", "-f", "-", "/var/lib/wsm")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Errorf("the token is under /var/lib/wsm, or grep failed: %q (%v)", strings.TrimSpace(out), err)
	}
	journal, err := run(ctx, "journalctl", "-u", "wsm", "--no-pager", "-o", "cat")
	if err != nil {
		t.Fatal("journalctl failed")
	}
	if strings.Contains(journal, tok) {
		t.Error("the token is in the wsm journal")
	}

	sqlDB := openDB(ctx, t)
	defer sqlDB.Close()
	var stored string
	if err := sqlDB.QueryRowContext(ctx, `SELECT mcp_token_hash FROM sandboxes WHERE id = ?`, s.a.sandboxID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok))
	if stored != hex.EncodeToString(sum[:]) {
		t.Errorf("mcp_token_hash of A is not the sha256 of its token")
	}
}

func TestMCPDeletedSandboxToken(t *testing.T) {
	ctx := ctxT(t)
	const slug = "mcp-gone"
	target := s.a.slug + "/" + slug
	wsm(ctx, "sandbox", "delete", target)
	if _, err := wsm(ctx, "sandbox", "create", target); err != nil {
		t.Fatal(err)
	}
	sb, ok, err := findSandbox(ctx, s.a.slug, slug)
	if err != nil || !ok {
		t.Fatalf("sandbox %s not found: %v", target, err)
	}
	env, err := containerEnv(ctx, s.a, sandbox.WorkContainer(sb.ID))
	if err != nil {
		t.Fatal(err)
	}
	tok := env["WSM_MCP_TOKEN"]
	if len(tok) != mcptoken.Length {
		t.Fatalf("token of %s has %d characters", target, len(tok))
	}
	// Control: the token works while the sandbox exists.
	r := mcpFromSandbox(ctx, t, s.a, mcpRequest{Op: "raw", URL: mcptoken.URL(sb.ID), Token: tok})
	if r.Status != http.StatusOK {
		t.Errorf("control: token of %s before delete: status %d (%s), want 200", target, r.Status, r.Error)
	}
	if _, err := wsm(ctx, "sandbox", "delete", target); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{mcptoken.URL(sb.ID), mcptoken.URL(s.a.sandboxID)} {
		r = mcpFromSandbox(ctx, t, s.a, mcpRequest{Op: "raw", URL: url, Token: tok})
		if r.Status != http.StatusUnauthorized {
			t.Errorf("deleted sandbox's token on %s: status %d (%s), want 401", url, r.Status, r.Error)
		}
	}
}

// TestMCPAuditChainIntact stays the last test in this file: it checks the
// events the tests above caused.
func TestMCPAuditChainIntact(t *testing.T) {
	ctx := ctxT(t)
	out, err := wsm(ctx, "audit", "verify")
	if err != nil || !strings.HasPrefix(out, "OK") {
		t.Errorf("wsm audit verify: %q, %v", out, err)
	}
	evs := auditAfter(ctx, t, 0)
	want := map[string]bool{
		"sandbox_info/ok/mcp:" + s.a.sandboxID:       false,
		"service_enable/denied/mcp:" + s.a.sandboxID: false,
		"http:POST/denied/mcp:unauthenticated":       false,
		"http:POST/denied/mcp:" + s.a.sandboxID:      false,
	}
	for _, e := range evs {
		k := e.Tool + "/" + e.Result + "/" + e.Actor
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("no audit event %s", k)
		}
	}
}
