package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/JCombee/wsm/internal/audit"
)

// Auditor stores audit events; *audit.Log is one. Append must not return
// before the event is stored.
type Auditor interface {
	Append(ctx context.Context, e audit.Event) (audit.Record, error)
}

// Limits on what an audit event keeps of a request.
const (
	maxAuditPath = 256
	maxAuditArgs = 16 << 10
	maxNameLen   = 64
)

// Actor of a request without a valid token.
const actorUnauthenticated = "mcp:unauthenticated"

var (
	toolNamePattern   = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	rpcMethodPattern  = regexp.MustCompile(`^[A-Za-z0-9_/]{1,64}$`)
	httpMethodPattern = regexp.MustCompile(`^[A-Z]{1,16}$`)
	protocolPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// record collects what one request's audit event says. Every layer of the
// handler chain fills in what it learns.
type record struct {
	httpMethod string
	path       string
	protocol   string
	sessionHdr bool
	authScheme string

	sandboxID string // from the token; empty without a valid token
	parsed    bool   // the body was a JSON-RPC object
	rpc       rpcRequest

	denied bool   // one of our layers refused the request
	failed bool   // a tool failed although the HTTP request succeeded
	detail string // why, or what a tool did
}

func (r *record) deny(detail string) {
	r.denied, r.detail = true, detail
}

func (r *record) fail(detail string) {
	r.failed, r.detail = true, detail
}

type recordKey struct{}

func recordFrom(ctx context.Context) *record {
	rec, _ := ctx.Value(recordKey{}).(*record)
	return rec
}

// newRecord describes a request from its method, path and headers. It
// keeps whether a session and an Authorization header were present, and
// the scheme, but never their values.
func newRecord(r *http.Request) *record {
	rec := &record{
		httpMethod: r.Method,
		path:       auditPath(r.URL.EscapedPath()),
		sessionHdr: r.Header.Get("Mcp-Session-Id") != "",
		authScheme: authScheme(r.Header.Get("Authorization")),
	}
	if p := r.Header.Get("Mcp-Protocol-Version"); protocolPattern.MatchString(p) {
		rec.protocol = p
	}
	return rec
}

func authScheme(h string) string {
	if h == "" {
		return "none"
	}
	scheme, _, _ := strings.Cut(strings.TrimSpace(h), " ")
	switch strings.ToLower(scheme) {
	case "bearer":
		return "bearer"
	case "basic":
		return "basic"
	default:
		return "other"
	}
}

func auditPath(p string) string {
	p = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, p)
	if len(p) > maxAuditPath {
		p = p[:maxAuditPath]
	}
	return p
}

// tool returns the event's tool: the tool name for tools/call, rpc:<method>
// for other JSON-RPC requests, and http:<METHOD> without a JSON-RPC body.
func (r *record) tool() string {
	if r.parsed && r.rpc.Method != "" {
		if r.rpc.Method == "tools/call" {
			if toolNamePattern.MatchString(r.rpc.ToolName) {
				return r.rpc.ToolName
			}
			return "invalid"
		}
		if rpcMethodPattern.MatchString(r.rpc.Method) {
			return "rpc:" + r.rpc.Method
		}
		return "rpc:invalid"
	}
	if httpMethodPattern.MatchString(r.httpMethod) {
		return "http:" + r.httpMethod
	}
	return "http:invalid"
}

// auditArgs are an event's arguments. The keys avoid the words that the
// audit log redacts ("session", "authorization", "token"), so the
// informative values stay; the values never hold a secret.
type auditArgs struct {
	HTTPMethod    string          `json:"http_method"`
	Path          string          `json:"path"`
	Status        int             `json:"status"`
	Protocol      string          `json:"protocol_version,omitempty"`
	SIDPresent    bool            `json:"mcp_sid_present"`
	AuthScheme    string          `json:"auth_scheme"`
	RPCMethod     string          `json:"rpc_method,omitempty"`
	ToolArguments json.RawMessage `json:"arguments,omitempty"`
}

func (r *record) event(status int) (audit.Event, error) {
	if status == http.StatusUnauthorized && !r.denied {
		// Only RequireBearerToken answers 401: the token was missing,
		// malformed or unknown.
		r.sandboxID = ""
		r.deny("no valid bearer token")
	}
	e := audit.Event{Actor: actorUnauthenticated, Tool: r.tool(), Detail: r.detail}
	if r.sandboxID != "" {
		e.Actor, e.SandboxID = "mcp:"+r.sandboxID, r.sandboxID
	}
	switch {
	case r.denied:
		e.Result = audit.ResultDenied
	case r.failed, status >= 400:
		e.Result = audit.ResultError
	default:
		e.Result = audit.ResultOK
	}
	if e.Detail == "" {
		e.Detail = http.StatusText(status)
	}
	args := auditArgs{
		HTTPMethod: r.httpMethod,
		Path:       r.path,
		Status:     status,
		Protocol:   r.protocol,
		SIDPresent: r.sessionHdr,
		AuthScheme: r.authScheme,
	}
	if !httpMethodPattern.MatchString(args.HTTPMethod) {
		args.HTTPMethod = "invalid"
	}
	if r.parsed {
		if r.rpc.Method != "" {
			args.RPCMethod = "invalid"
			if rpcMethodPattern.MatchString(r.rpc.Method) {
				args.RPCMethod = r.rpc.Method
			}
		}
		if len(r.rpc.Arguments) > maxAuditArgs {
			args.ToolArguments, _ = json.Marshal(map[string]any{"truncated": true, "bytes": len(r.rpc.Arguments)})
		} else if len(r.rpc.Arguments) > 0 {
			args.ToolArguments = r.rpc.Arguments
		}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return audit.Event{}, err
	}
	e.Args = b
	return e, nil
}

// auditingWriter holds back the response until its audit event is stored:
// the first WriteHeader, Write or Flush appends the event. When the append
// fails, the client gets a 500 instead of the real response.
type auditingWriter struct {
	http.ResponseWriter
	ctx    context.Context
	audit  Auditor
	rec    *record
	logf   func(format string, args ...any)
	done   bool
	failed bool
}

var errAuditFailed = errors.New("audit failed")

// commit appends the event once, with status, and reports whether the real
// response may go out.
func (w *auditingWriter) commit(status int) bool {
	if w.done {
		return !w.failed
	}
	w.done = true
	e, err := w.rec.event(status)
	if err == nil {
		_, err = w.audit.Append(w.ctx, e)
	}
	if err != nil {
		w.failed = true
		w.logf("mcp: audit append failed, refusing the request: %v", err)
		h := w.ResponseWriter.Header()
		for k := range h {
			delete(h, k)
		}
		http.Error(w.ResponseWriter, "audit unavailable", http.StatusInternalServerError)
		return false
	}
	return true
}

func (w *auditingWriter) WriteHeader(code int) {
	if w.commit(code) {
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *auditingWriter) Write(b []byte) (int, error) {
	if !w.commit(http.StatusOK) {
		return 0, errAuditFailed
	}
	return w.ResponseWriter.Write(b)
}

// Flush and Unwrap keep server-sent events working through the wrapper.
func (w *auditingWriter) Flush() {
	if !w.commit(http.StatusOK) {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *auditingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
