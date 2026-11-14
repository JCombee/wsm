// Package api is the manager's HTTP API. In M1 it serves the CLI over the
// unix socket /run/wsm/api.sock, which only group wsm-admin can open.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/sandbox"
	"github.com/JCombee/wsm/internal/terminal"
)

// DefaultSocket is where "wsm serve" listens and the CLI connects.
const DefaultSocket = "/run/wsm/api.sock"

// maxRequestBody caps a JSON request body.
const maxRequestBody = 64 << 10

// maxProjectBody caps the body of POST /v1/projects, which can carry a
// Containerfile of db.MaxContainerfile bytes. JSON escaping can make each
// byte up to six bytes long.
const maxProjectBody = 6*db.MaxContainerfile + maxRequestBody

// Project is a project in API responses.
type Project struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	LinuxUser string `json:"linux_user"`
	LinuxUID  int    `json:"linux_uid"`
	// ImageSource is "registry" or "containerfile".
	ImageSource string `json:"image_source"`
	// Image is the image sandboxes run: the registry reference, or the tag
	// wsm builds from the Containerfile.
	Image     string `json:"image"`
	CPUMillis int64  `json:"cpu_millis"`
	MemBytes  int64  `json:"mem_bytes"`
	PidsLimit int64  `json:"pids_limit"`
	// RepoURL and BaseBranch are empty for a project without a repository.
	RepoURL    string    `json:"repo_url,omitempty"`
	BaseBranch string    `json:"base_branch,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// DeployKey is the response of GET /v1/projects/{project}/key: the public
// part only.
type DeployKey struct {
	PublicKey string `json:"public_key"`
}

// Sandbox is a sandbox in API responses.
type Sandbox struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Slug    string `json:"slug"`
	Status  string `json:"status"`
	Pod     string `json:"pod"`
	// Branch is agent/<slug> in a project with a repository.
	Branch    string    `json:"branch,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateProjectRequest is the body of POST /v1/projects. It has either an
// Image from a public registry or a Containerfile, not both.
type CreateProjectRequest struct {
	Slug          string `json:"slug"`
	Name          string `json:"name,omitempty"`
	Image         string `json:"image,omitempty"`
	Containerfile string `json:"containerfile,omitempty"`
	CPUMillis     int64  `json:"cpu_millis,omitempty"`
	MemBytes      int64  `json:"mem_bytes,omitempty"`
	PidsLimit     int64  `json:"pids_limit,omitempty"`
	// RepoURL is an SSH repository; BaseBranch defaults to main.
	RepoURL    string `json:"repo_url,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// ProjectImage is the response of POST /v1/projects/{project}/image.
type ProjectImage struct {
	Image string `json:"image"`
	ID    string `json:"id"`
}

// CreateSandboxRequest is the body of POST /v1/projects/{project}/sandboxes.
type CreateSandboxRequest struct {
	Slug string `json:"slug"`
}

// Error is an error response.
type Error struct {
	Error string `json:"error"`
}

// Server serves the API.
type Server struct {
	Manager *sandbox.Manager
	// UserName derives a project's Linux user name from its UID.
	UserName func(uid int) string
	Logf     func(format string, args ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Handler returns the API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/projects", s.listProjects)
	mux.HandleFunc("POST /v1/projects", s.createProject)
	mux.HandleFunc("DELETE /v1/projects/{project}", s.deleteProject)
	mux.HandleFunc("POST /v1/projects/{project}/image", s.projectImage)
	mux.HandleFunc("GET /v1/projects/{project}/key", s.deployKey)
	mux.HandleFunc("GET /v1/sandboxes", s.listSandboxes)
	mux.HandleFunc("POST /v1/projects/{project}/sandboxes", s.createSandbox)
	mux.HandleFunc("POST /v1/projects/{project}/sandboxes/{sandbox}/start", s.startSandbox)
	mux.HandleFunc("POST /v1/projects/{project}/sandboxes/{sandbox}/stop", s.stopSandbox)
	mux.HandleFunc("DELETE /v1/projects/{project}/sandboxes/{sandbox}", s.deleteSandbox)
	mux.HandleFunc("GET /v1/projects/{project}/sandboxes/{sandbox}/terminal", s.terminal)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, sandbox.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, db.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, db.ErrConflict), errors.Is(err, sandbox.ErrState):
		status = http.StatusConflict
	}
	s.logf("%s %s: %d: %v", r.Method, r.URL.Path, status, err)
	writeJSON(w, status, Error{Error: err.Error()})
}

// decodeBody strictly parses a JSON request body of at most limit bytes
// into v.
func decodeBody(r *http.Request, limit int64, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return fmt.Errorf("%w: read body: %v", sandbox.ErrInvalid, err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%w: body exceeds %d bytes", sandbox.ErrInvalid, limit)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: body: %v", sandbox.ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after the body", sandbox.ErrInvalid)
	}
	return nil
}

func toProject(p db.Project) Project {
	return Project{
		ID: p.ID, Slug: p.Slug, Name: p.Name, LinuxUser: p.LinuxUser, LinuxUID: p.LinuxUID,
		ImageSource: p.ImageSource, Image: sandbox.ProjectImage(p), CPUMillis: p.CPUMillis, MemBytes: p.MemBytes, PidsLimit: p.PidsLimit,
		RepoURL: p.RepoURL, BaseBranch: p.BaseBranch, CreatedAt: p.CreatedAt,
	}
}

func toSandbox(projectSlug string, sb db.Sandbox) Sandbox {
	return Sandbox{ID: sb.ID, Project: projectSlug, Slug: sb.Slug, Status: sb.Status,
		Pod: sb.PodName, Branch: sb.Branch, CreatedAt: sb.CreatedAt}
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.Manager.Projects(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]Project, 0, len(ps))
	for _, p := range ps {
		out = append(out, toProject(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req CreateProjectRequest
	if err := decodeBody(r, maxProjectBody, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.Manager.CreateProject(r.Context(), sandbox.ProjectSpec{
		Slug: req.Slug, Name: req.Name, ImageRef: req.Image, Containerfile: req.Containerfile,
		CPUMillis: req.CPUMillis, MemBytes: req.MemBytes, PidsLimit: req.PidsLimit,
		RepoURL: req.RepoURL, BaseBranch: req.BaseBranch,
	}, s.UserName)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProject(p))
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.Manager.DeleteProject(r.Context(), r.PathValue("project")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// projectImage builds or pulls a project's image now and waits for it.
func (s *Server) projectImage(w http.ResponseWriter, r *http.Request) {
	ref, id, err := s.Manager.RefreshImage(r.Context(), r.PathValue("project"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ProjectImage{Image: ref, ID: id})
}

// deployKey returns the public part of a project's deploy key.
func (s *Server) deployKey(w http.ResponseWriter, r *http.Request) {
	pub, err := s.Manager.DeployKey(r.Context(), r.PathValue("project"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, DeployKey{PublicKey: pub})
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectSlug := r.URL.Query().Get("project")
	sbs, err := s.Manager.Sandboxes(ctx, projectSlug)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	slugs := map[string]string{}
	ps, err := s.Manager.Projects(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	for _, p := range ps {
		slugs[p.ID] = p.Slug
	}
	out := make([]Sandbox, 0, len(sbs))
	for _, sb := range sbs {
		out = append(out, toSandbox(slugs[sb.ProjectID], sb))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req CreateSandboxRequest
	if err := decodeBody(r, maxRequestBody, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	project := r.PathValue("project")
	sb, err := s.Manager.CreateSandbox(r.Context(), project, req.Slug)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSandbox(project, sb))
}

func (s *Server) sandboxAction(w http.ResponseWriter, r *http.Request,
	fn func(ctx context.Context, project, sandbox string) error) {
	if err := fn(r.Context(), r.PathValue("project"), r.PathValue("sandbox")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startSandbox(w http.ResponseWriter, r *http.Request) {
	s.sandboxAction(w, r, s.Manager.StartSandbox)
}

func (s *Server) stopSandbox(w http.ResponseWriter, r *http.Request) {
	s.sandboxAction(w, r, s.Manager.StopSandbox)
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	s.sandboxAction(w, r, s.Manager.DeleteSandbox)
}

// terminal upgrades to a WebSocket and attaches it to tmux in the sandbox.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, _ := strconv.Atoi(q.Get("rows"))
	cols, _ := strconv.Atoi(q.Get("cols"))
	if !terminal.ValidSize(rows, cols) {
		rows, cols = 24, 80
	}
	pc, container, err := s.Manager.ShellTarget(r.Context(), r.PathValue("project"), r.PathValue("sandbox"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer pc.Close()
	// Accept checks the Origin header against the Host when a browser sends
	// one; the CLI sends none.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logf("terminal: accept: %v", err)
		return
	}
	defer ws.CloseNow()
	err = terminal.Serve(r.Context(), ws, pc, terminal.Session{
		Container: container, Cmd: terminal.ShellCmd, WorkingDir: sandbox.WorkspaceDir,
		Rows: rows, Cols: cols,
	})
	if err != nil {
		s.logf("terminal %s/%s: %v", r.PathValue("project"), r.PathValue("sandbox"), err)
		ws.Close(websocket.StatusInternalError, "terminal error")
		return
	}
	ws.Close(websocket.StatusNormalClosure, "")
}
