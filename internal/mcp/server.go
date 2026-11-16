// Package mcp is the manager's MCP server (plan section 3.9, ADR 0011 and
// ADR 0012). Every sandbox reaches it at http://10.255.0.1:7443/mcp/<id>
// with its own bearer token, and the token is the only source of the
// sandbox's identity.
//
// The handler chain, from the outside in:
//
//  1. audit: every HTTP request becomes one audit event, stored before the
//     first byte of the response goes out;
//  2. bearer token, on every path: the token's hash must belong to a
//     sandbox;
//  3. path check: only /mcp/<own sandbox id> passes;
//  4. body: one JSON object, at most 1 MiB, without duplicate keys;
//  5. policy: tools/call is allowed only for the known, enabled tools;
//  6. the SDK's Streamable HTTP handler, stateless.
package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/mcptoken"
)

// MaxBodyBytes limits every request body.
const MaxBodyBytes = 1 << 20

// Store is what the MCP server needs from the database; *db.Store is one.
type Store interface {
	SandboxByTokenHash(ctx context.Context, hash string) (db.Project, db.Sandbox, error)
	TouchSandbox(ctx context.Context, id string) error
}

// Server serves MCP to the sandboxes.
type Server struct {
	Store Store
	Audit Auditor
	// Services backs services_available, service_enable and the services
	// of sandbox_info. Nil means no services: empty lists and a denial.
	Services Services
	Logf     func(format string, args ...any)
	Version  string
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Keys of auth.TokenInfo.Extra. The verifier fills them from the sandbox's
// row; the tools read them only from there.
const (
	extraProject = "project"
	extraSandbox = "sandbox"
	extraBranch  = "branch"
)

// verify maps a bearer token to its sandbox. It returns only
// auth.ErrInvalidToken, so the client always gets the same 401, and it
// never logs the token.
func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if !mcptoken.ValidToken(token) {
		return nil, auth.ErrInvalidToken
	}
	hash := mcptoken.Hash(token)
	p, sb, err := s.Store.SandboxByTokenHash(ctx, hash)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			s.logf("mcp: token lookup failed: %v", err)
		}
		return nil, auth.ErrInvalidToken
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(sb.MCPTokenHash)) != 1 {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		UserID: sb.ID,
		Extra:  map[string]any{extraProject: p.Slug, extraSandbox: sb.Slug, extraBranch: sb.Branch},
	}, nil
}

// Handler returns the whole handler chain.
func (s *Server) Handler() http.Handler {
	streamable := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return s.newMCPServer() },
		&sdk.StreamableHTTPOptions{Stateless: true, MaxRequestBodyBytes: MaxBodyBytes})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordFrom(r.Context())
		ti := auth.TokenInfoFromContext(r.Context())
		if ti == nil || ti.UserID == "" {
			// RequireBearerToken never lets this happen.
			rec.deny("no token info")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rec.sandboxID = ti.UserID
		if !s.checkPath(w, r, rec, ti.UserID) {
			return
		}
		if err := s.Store.TouchSandbox(r.Context(), ti.UserID); err != nil {
			s.logf("mcp: record activity of sandbox %s: %v", ti.UserID, err)
		}
		if r.Method == http.MethodPost {
			if !readBody(w, r, rec) {
				return
			}
			if reason := policyDeny(rec.rpc); reason != "" {
				rec.deny(reason)
				writeDenied(w, rec.rpc, reason)
				return
			}
		}
		streamable.ServeHTTP(w, r)
	})
	authed := auth.RequireBearerToken(s.verify, &auth.RequireBearerTokenOptions{
		// Tokens are opaque and live as long as their sandbox.
		AllowMissingExpiration: true,
	})(inner)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := newRecord(r)
		aw := &auditingWriter{
			ResponseWriter: w,
			ctx:            context.WithoutCancel(r.Context()),
			audit:          s.Audit,
			rec:            rec,
			logf:           s.logf,
		}
		ctx := context.WithValue(r.Context(), recordKey{}, rec)
		authed.ServeHTTP(aw, r.WithContext(ctx))
		// A handler that wrote nothing still gets its event before the
		// (empty) response goes out.
		aw.commit(http.StatusOK)
	})
}

// checkPath lets only /mcp/<own sandbox id> through: another sandbox's
// path gets 403, any other path 404.
func (s *Server) checkPath(w http.ResponseWriter, r *http.Request, rec *record, sandboxID string) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, mcptoken.PathPrefix)
	switch {
	case ok && r.URL.RawPath == "" && rest == sandboxID:
		return true
	case ok && rest != "" && !strings.Contains(rest, "/"):
		rec.deny("path names another sandbox")
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		rec.deny("unknown path")
		http.NotFound(w, r)
	}
	return false
}

// readBody reads and checks a POST body once, and puts it back for the
// SDK.
func readBody(w http.ResponseWriter, r *http.Request, rec *record) bool {
	if r.ContentLength > MaxBodyBytes {
		rec.deny("body too large")
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		rec.deny("read body failed")
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	if len(body) > MaxBodyBytes {
		rec.deny("body too large")
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return false
	}
	req, err := parseBody(body)
	if err != nil {
		reason := "invalid body"
		switch {
		case errors.Is(err, errBatch):
			reason = "batch request"
		case errors.Is(err, errDupKey):
			reason = "duplicate key in body"
		}
		rec.deny(reason)
		http.Error(w, "bad request: "+reason, http.StatusBadRequest)
		return false
	}
	rec.parsed, rec.rpc = true, req
	if req.Method == "initialize" && rec.protocol == "" {
		var p struct {
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &p) == nil && protocolPattern.MatchString(p.Params.ProtocolVersion) {
			rec.protocol = p.Params.ProtocolVersion
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return true
}

// policyDeny returns why a request is refused, or "" when it may go to
// the SDK. A tool call is allowed only for the known tools; each checks
// its own arguments.
func policyDeny(req rpcRequest) string {
	if req.Method != "tools/call" {
		return ""
	}
	switch req.ToolName {
	case toolSandboxInfo, toolServicesAvailable, toolServiceEnable:
		return ""
	default:
		return "unknown tool"
	}
}

// writeDenied answers a refused tool call with a tool result that has
// isError and a text starting with "denied:", as the tool would.
func writeDenied(w http.ResponseWriter, req rpcRequest, reason string) {
	if !validRPCID(req.ID) {
		http.Error(w, "bad request: tool call without a valid id", http.StatusBadRequest)
		return
	}
	resp := struct {
		JSONRPC string              `json:"jsonrpc"`
		ID      json.RawMessage     `json:"id"`
		Result  *sdk.CallToolResult `json:"result"`
	}{"2.0", req.ID, deniedResult(reason)}
	b, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

// validRPCID reports whether id is a JSON-RPC id: a string or a number.
func validRPCID(id json.RawMessage) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	var v any
	if err := json.Unmarshal(id, &v); err != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

// HTTP server limits (ADR 0011, point 5).
const (
	ReadHeaderTimeout = 10 * time.Second
	ReadTimeout       = 30 * time.Second
	// WriteTimeout is long enough for an SSE stream; clients reconnect.
	WriteTimeout   = 10 * time.Minute
	IdleTimeout    = 2 * time.Minute
	MaxHeaderBytes = 16 << 10
)

// NewHTTPServer returns the http.Server for the MCP handler, with its
// limits.
func NewHTTPServer(h http.Handler, errorLog *log.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		ErrorLog:          errorLog,
	}
}

// Listen listens on the MCP address on wsm0, and nowhere else.
func Listen(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp4", mcptoken.Addr())
}
