package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/JCombee/wsm/internal/fsproto"
)

// errUnavailable marks a feature this server was started without.
var errUnavailable = errors.New("not available on this server")

// maxWriteBody caps PUT .../files/content: base64 makes content a third
// larger.
const maxWriteBody = fsproto.MaxFile*2 + maxRequestBody

// FileContent is the body of PUT .../files/content and the response of
// GET .../files/content. Content is base64 in JSON.
type FileContent struct {
	Content []byte `json:"content"`
}

// FileList is the response of GET .../files/list.
type FileList struct {
	Path      string          `json:"path"`
	Entries   []fsproto.Entry `json:"entries"`
	Truncated bool            `json:"truncated,omitempty"`
}

// files serves GET .../files/{list,stat,content}?path=P.
func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	op := r.PathValue("op")
	switch op {
	case "list", "stat":
	case "content":
		op = fsproto.OpRead
	default:
		http.NotFound(w, r)
		return
	}
	resp, err := s.Manager.FS(r.Context(), r.PathValue("project"), r.PathValue("sandbox"), op, fsproto.Request{Path: path})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	switch op {
	case fsproto.OpList:
		writeJSON(w, http.StatusOK, FileList{Path: path, Entries: resp.Entries, Truncated: resp.Truncated})
	case fsproto.OpStat:
		writeJSON(w, http.StatusOK, resp.Entry)
	default:
		if resp.Content == nil {
			resp.Content = []byte{}
		}
		writeJSON(w, http.StatusOK, FileContent{Content: resp.Content})
	}
}

// writeFile serves PUT .../files/content?path=P: an atomic write.
func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	var body FileContent
	if err := decodeBody(r, maxWriteBody, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	resp, err := s.Manager.FS(r.Context(), r.PathValue("project"), r.PathValue("sandbox"), fsproto.OpWrite,
		fsproto.Request{Path: r.URL.Query().Get("path"), Content: body.Content})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp.Entry)
}

// EnabledService is a service a sandbox enabled, without its password.
type EnabledService struct {
	Block    string `json:"block"`
	Tag      string `json:"tag"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user,omitempty"`
	Database string `json:"database,omitempty"`
}

// sandboxServices lists the services a sandbox enabled, without secrets.
func (s *Server) sandboxServices(w http.ResponseWriter, r *http.Request) {
	_, sb, err := s.Manager.Lookup(r.Context(), r.PathValue("project"), r.PathValue("sandbox"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	enabled, err := s.Broker.Enabled(r.Context(), sb.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]EnabledService, 0, len(enabled))
	for _, e := range enabled {
		out = append(out, EnabledService{Block: e.Block, Tag: e.Tag, Host: e.Host, Port: e.Port,
			User: e.User, Database: e.Database})
	}
	writeJSON(w, http.StatusOK, out)
}

// revokeService takes one service away from a sandbox.
func (s *Server) revokeService(w http.ResponseWriter, r *http.Request) {
	_, sb, err := s.Manager.Lookup(r.Context(), r.PathValue("project"), r.PathValue("sandbox"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Broker.Revoke(r.Context(), sb.ID, r.PathValue("block")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AuditEvent is an audit record in API responses.
type AuditEvent struct {
	Seq       int64  `json:"seq"`
	TS        string `json:"ts"`
	Actor     string `json:"actor"`
	SandboxID string `json:"sandbox_id,omitempty"`
	Tool      string `json:"tool"`
	Args      string `json:"args"`
	Result    string `json:"result"`
	Detail    string `json:"detail,omitempty"`
	Hash      string `json:"hash"`
}

// listAudit serves GET /v1/audit?before=SEQ&limit=N&sandbox=ID, newest
// first.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	if s.Audit == nil {
		s.writeError(w, r, errUnavailable)
		return
	}
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	recs, err := s.Audit.List(r.Context(), before, limit, q.Get("sandbox"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]AuditEvent, 0, len(recs))
	for _, rec := range recs {
		out = append(out, AuditEvent{Seq: rec.Seq, TS: rec.TS, Actor: rec.Actor, SandboxID: rec.SandboxID,
			Tool: rec.Tool, Args: rec.ArgsJSON, Result: rec.Result, Detail: rec.Detail, Hash: rec.Hash})
	}
	writeJSON(w, http.StatusOK, out)
}

// EnrollLink is the response of POST /v1/auth/enroll.
type EnrollLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	if s.Enroll == nil {
		s.writeError(w, r, errUnavailable)
		return
	}
	link, expires, err := s.Enroll(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, EnrollLink{URL: link, ExpiresAt: expires})
}
