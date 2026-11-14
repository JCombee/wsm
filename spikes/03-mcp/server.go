package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxBodyBytes limits every MCP request body. Tool arguments are small.
const maxBodyBytes = 1 << 20

// sandboxIDPattern matches the sandbox IDs wsm hands out (internal/db).
var sandboxIDPattern = regexp.MustCompile(`^[a-z2-7]{12}$`)

// Config is the server's token file. It holds only token hashes.
type Config struct {
	Sandboxes []SandboxToken `json:"sandboxes"`
}

// SandboxToken binds a sandbox ID to the sha256 of its token.
type SandboxToken struct {
	ID          string `json:"id"`
	TokenSHA256 string `json:"token_sha256"` // hex
}

type tokenEntry struct {
	id   string
	hash [sha256.Size]byte
}

func loadConfig(path string) ([]tokenEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return parseTokens(c)
}

func parseTokens(c Config) ([]tokenEntry, error) {
	var out []tokenEntry
	seen := map[string]bool{}
	for _, s := range c.Sandboxes {
		if !sandboxIDPattern.MatchString(s.ID) {
			return nil, fmt.Errorf("invalid sandbox id %q", s.ID)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("duplicate sandbox id %q", s.ID)
		}
		seen[s.ID] = true
		h, err := hex.DecodeString(s.TokenSHA256)
		if err != nil || len(h) != sha256.Size {
			return nil, fmt.Errorf("sandbox %s: token_sha256 must be 64 hex characters", s.ID)
		}
		e := tokenEntry{id: s.ID}
		copy(e.hash[:], h)
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, errors.New("config has no sandboxes")
	}
	return out, nil
}

// verifier returns a TokenVerifier that maps a token to its sandbox. It
// compares the token's hash with every entry in constant time and only ever
// returns auth.ErrInvalidToken, so the middleware answers 401 and never
// echoes a message.
func verifier(tokens []tokenEntry) auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		got := sha256.Sum256([]byte(token))
		match := -1
		for i := range tokens {
			if subtle.ConstantTimeCompare(got[:], tokens[i].hash[:]) == 1 {
				match = i
			}
		}
		if match < 0 {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: tokens[match].id}, nil
	}
}

// serverOptions configure the MCP handler.
type serverOptions struct {
	Stateless bool
	Audit     AuditWriter
	Logf      func(format string, args ...any)
}

type auditKey struct{}

// newHandler returns the whole HTTP handler of the MCP server: audit, bearer
// token, path check and the Streamable HTTP transport.
func newHandler(tokens []tokenEntry, opts serverOptions) http.Handler {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	server := newMCPServer()
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:           opts.Stateless,
			MaxRequestBodyBytes: maxBodyBytes,
			SessionTimeout:      30 * time.Minute,
		})
	// The path must name the sandbox the token belongs to.
	pathCheck := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := r.Context().Value(auditKey{}).(*Record)
		ti := auth.TokenInfoFromContext(r.Context())
		if ti == nil {
			rec.Reason = "no token info"
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		rec.Sandbox = ti.UserID
		if r.PathValue("sandbox") != ti.UserID {
			rec.Reason = "path does not match token"
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if reason := policyDeny(rec.RPCMethod, rec.Tool); reason != "" {
			// The tool itself answers with isError; the audit records the
			// decision before that answer goes out.
			rec.Decision, rec.Reason = "deny", reason
		}
		streamable.ServeHTTP(w, r)
	})
	authed := auth.RequireBearerToken(verifier(tokens), &auth.RequireBearerTokenOptions{
		// Our tokens are opaque and live as long as the sandbox.
		AllowMissingExpiration: true,
	})(pathCheck)

	mux := http.NewServeMux()
	mux.Handle("/mcp/{sandbox}", authed)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		r.Context().Value(auditKey{}).(*Record).Reason = "unknown path"
		http.NotFound(w, r)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &Record{
			Time:     time.Now().UTC(),
			Method:   r.Method,
			Path:     r.URL.Path,
			Protocol: r.Header.Get("Mcp-Protocol-Version"),
			Session:  r.Header.Get("Mcp-Session-Id") != "",
		}
		aw := &auditingWriter{ResponseWriter: w, audit: opts.Audit, rec: rec, errLogf: opts.Logf}
		if r.Body != nil && r.Method == http.MethodPost {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
			if err != nil {
				rec.Reason = "read body failed"
				http.Error(aw, "bad request", http.StatusBadRequest)
				return
			}
			if len(body) > maxBodyBytes {
				rec.Reason = "body too large"
				http.Error(aw, "request too large", http.StatusRequestEntityTooLarge)
				return
			}
			describeRPC(body, rec)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		mux.ServeHTTP(aw, r.WithContext(context.WithValue(r.Context(), auditKey{}, rec)))
		// A handler that wrote nothing still gets its record before the
		// response goes out.
		aw.commit(http.StatusOK)
	})
}

// policyDeny returns why a call is denied, or "" when it is allowed.
func policyDeny(rpcMethod, tool string) string {
	if rpcMethod == "tools/call" && tool == "service_enable" {
		return "service_enable is not part of this spike"
	}
	return ""
}

// describeRPC fills the JSON-RPC fields of rec from a request body. The body
// is untrusted: unknown shapes are ignored, the transport rejects them.
func describeRPC(body []byte, rec *Record) {
	var msg struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Params struct {
			Name            string `json:"name"`
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil {
		rec.RPCMethod = "(unparsed)"
		return
	}
	rec.RPCMethod = clip(msg.Method)
	if len(msg.ID) > 0 && len(msg.ID) <= 64 {
		rec.RPCID = string(msg.ID)
	}
	if msg.Method == "tools/call" {
		rec.Tool = clip(msg.Params.Name)
	}
	if msg.Method == "initialize" && rec.Protocol == "" {
		rec.Protocol = clip(msg.Params.ProtocolVersion)
	}
}

func clip(s string) string {
	if len(s) > 128 {
		return s[:128]
	}
	return s
}

type noArgs struct{}

type serviceArgs struct {
	Service string `json:"service" jsonschema:"name of the service to enable, from services_available"`
}

// newMCPServer registers the three stub tools. None takes a project or
// sandbox argument: the identity comes only from the token.
func newMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "wsm-spike", Version: "0.0.1"}, nil)
	f := false
	mcp.AddTool(s, &mcp.Tool{
		Name:        "sandbox_info",
		Description: "Show which sandbox this connection belongs to.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &f},
	}, func(_ context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		id := identity(req)
		if id == "" {
			return errorResult("no sandbox identity"), nil, nil
		}
		return textResult("sandbox " + id), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "services_available",
		Description: "List the shared services this sandbox could enable.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &f},
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		return textResult("mariadb"), nil, nil
	})
	t := true
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_enable",
		Description: "Enable a shared service for this sandbox and return its credentials.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &f, IdempotentHint: true, OpenWorldHint: &t},
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ serviceArgs) (*mcp.CallToolResult, any, error) {
		return errorResult("denied: service_enable is not available in this spike (niet in deze spike)"), nil, nil
	})
	return s
}

func identity(req *mcp.CallToolRequest) string {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return ""
	}
	return req.Extra.TokenInfo.UserID
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errorResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func runServe(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "10.255.0.1:7443", "listen address")
	configPath := fs.String("config", "", "token config (JSON with token hashes)")
	auditPath := fs.String("audit", "", "audit log (JSONL)")
	stateless := fs.Bool("stateless", false, "serve without sessions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *auditPath == "" {
		return errors.New("-config and -audit are required")
	}
	tokens, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	audit, err := openFileAudit(*auditPath)
	if err != nil {
		return err
	}
	defer audit.Close()
	logger := log.New(stderr, "mcpspike serve: ", log.LstdFlags)
	srv := &http.Server{
		Handler:           newHandler(tokens, serverOptions{Stateless: *stateless, Audit: audit, Logf: logger.Printf}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Long enough for an SSE stream; clients reconnect after it.
		WriteTimeout:   10 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 16 << 10,
		ErrorLog:       logger,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	mode := "stateful"
	if *stateless {
		mode = "stateless"
	}
	logger.Printf("listening on %s (%s, %d sandboxes)", ln.Addr(), mode, len(tokens))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	logger.Printf("stopped")
	return nil
}

// tokenHash returns the hex sha256 of a token, as stored in the config.
func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
