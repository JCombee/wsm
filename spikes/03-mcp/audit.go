package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// Record is one audit line: one per HTTP request the MCP server sees,
// allowed or denied. It never holds a token or raw headers.
type Record struct {
	Time      time.Time `json:"time"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	RPCMethod string    `json:"rpc_method,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	RPCID     string    `json:"rpc_id,omitempty"`
	// Protocol is the MCP protocol version: the MCP-Protocol-Version header,
	// or protocolVersion from initialize.
	Protocol string `json:"protocol,omitempty"`
	// Session tells whether the request carried a session ID; the ID itself
	// is not logged.
	Session bool `json:"session"`
	// Auth is the kind of Authorization header: none, bearer or other.
	Auth string `json:"auth"`
	// Sandbox is the identity derived from the token; empty when the token
	// was missing or wrong.
	Sandbox  string `json:"sandbox,omitempty"`
	Decision string `json:"decision"` // allow or deny
	Reason   string `json:"reason,omitempty"`
	Status   int    `json:"status"`
}

// AuditWriter stores audit records. Write must not return before the
// record is durable; the server only answers after Write returned nil.
type AuditWriter interface {
	Write(Record) error
}

// fileAudit appends JSONL records to a file and syncs after each one.
type fileAudit struct {
	mu sync.Mutex
	f  *os.File
}

func openFileAudit(path string) (*fileAudit, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return &fileAudit{f: f}, nil
}

func (a *fileAudit) Write(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	b = append(b, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.f.Write(b); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	if err := a.f.Sync(); err != nil {
		return fmt.Errorf("sync audit log: %w", err)
	}
	return nil
}

func (a *fileAudit) Close() error { return a.f.Close() }

// auditingWriter holds back the response until the audit record is
// written: the first WriteHeader or Write writes the record with the
// status. If the audit write fails, the client gets a 500 instead of the
// real response.
type auditingWriter struct {
	http.ResponseWriter
	audit   AuditWriter
	rec     *Record
	done    bool // the record has been handled
	failed  bool // the audit write failed; drop the real response
	errLogf func(format string, args ...any)
}

// commit writes the audit record once, with status. It reports whether the
// real response may go out.
func (w *auditingWriter) commit(status int) bool {
	if w.done {
		return !w.failed
	}
	w.done = true
	w.rec.Status = status
	if w.rec.Decision == "" {
		w.rec.Decision = "allow"
		if status >= 400 {
			w.rec.Decision = "deny"
		}
	}
	if err := w.audit.Write(*w.rec); err != nil {
		w.failed = true
		w.errLogf("audit write failed, refusing request: %v", err)
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
		return 0, fmt.Errorf("audit failed")
	}
	return w.ResponseWriter.Write(b)
}

// Flush and Unwrap keep SSE working through the wrapper.
func (w *auditingWriter) Flush() {
	if w.done && w.failed {
		return
	}
	if !w.done && !w.commit(http.StatusOK) {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *auditingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
