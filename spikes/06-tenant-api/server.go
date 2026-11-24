package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Server serves one tenant socket per project in the configuration.
type Server struct {
	cfg   Config
	audit *Audit
	admin Admin
	log   *log.Logger
	// ready waits until a new sandbox accepts execs. Nil skips the wait.
	ready func(ctx context.Context, p Project, sb Sandbox) error
	// podman returns the exec runner for a project's Podman socket.
	podman func(socket string) Execer
}

// tenantHandler serves one project's socket. The project comes from the
// socket, never from the request.
type tenantHandler struct {
	s     *Server
	t     TenantConfig
	mu    sync.Mutex // serializes the count and the create of a sandbox
	execs atomic.Int32
}

// sandboxIDRE matches the IDs the server hands out. A control room can only
// address those: sandboxes the owner made with the CLI stay out of reach.
var sandboxIDRE = regexp.MustCompile(`^t[a-z0-9]{10}$`)

var (
	reqIDRE     = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	linuxUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	podNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

const (
	maxCreateBody = 16 << 10
	readyTimeout  = 90 * time.Second
	noSuchSandbox = "no such sandbox"
)

type apiError struct {
	Error string `json:"error"`
}

// TenantSandbox is a sandbox as the tenant API shows it.
type TenantSandbox struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type createRequest struct {
	Image     string `json:"image,omitempty"`
	CPUMillis int64  `json:"cpu_millis,omitempty"`
	MemBytes  int64  `json:"mem_bytes,omitempty"`
	PidsLimit int64  `json:"pids_limit,omitempty"`
}

// reqCtx carries one request and makes sure it is audited exactly once
// before its response, apart from the end line of an exec.
type reqCtx struct {
	w       http.ResponseWriter
	r       *http.Request
	h       *tenantHandler
	rec     Record
	audited bool
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAudit writes the request's line once. The caller must not act or
// answer when it fails.
func (rc *reqCtx) writeAudit() error {
	if rc.audited {
		return nil
	}
	if err := rc.h.s.audit.Write(rc.rec); err != nil {
		rc.h.s.log.Printf("project=%s route=%s: %v", rc.rec.Project, rc.rec.Route, err)
		return err
	}
	rc.audited = true
	return nil
}

// deny audits a refusal and answers it.
func (rc *reqCtx) deny(status int, reason string) {
	rc.finish("deny", status, reason)
}

// fail audits an error before any action and answers it.
func (rc *reqCtx) fail(status int, reason string) {
	rc.finish("error", status, reason)
}

func (rc *reqCtx) finish(decision string, status int, reason string) {
	rc.rec.Decision, rc.rec.Status, rc.rec.Reason = decision, status, reason
	if err := rc.writeAudit(); err != nil {
		writeJSON(rc.w, http.StatusServiceUnavailable, apiError{"audit unavailable"})
		return
	}
	writeJSON(rc.w, status, apiError{reason})
}

// allow audits the decision to act. It answers 503 and returns false when
// the line cannot be written: then the call is not carried out.
func (rc *reqCtx) allow() bool {
	rc.rec.Decision = "allow"
	if err := rc.writeAudit(); err != nil {
		writeJSON(rc.w, http.StatusServiceUnavailable, apiError{"audit unavailable"})
		return false
	}
	return true
}

// parseRoute maps a request to a route name and a sandbox ID without side
// effects. status is 0, 404 for an unknown path or 405 for a wrong method.
func parseRoute(method, path string) (route, id string, status int) {
	const prefix = "/v1/tenant/"
	if !strings.HasPrefix(path, prefix) {
		return "unknown", "", http.StatusNotFound
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	want := func(name, m string) (string, string, int) {
		if method != m {
			return name, id, http.StatusMethodNotAllowed
		}
		return name, id, 0
	}
	switch {
	case len(parts) == 1 && parts[0] == "whoami":
		return want("whoami", http.MethodGet)
	case len(parts) == 1 && parts[0] == "sandboxes":
		switch method {
		case http.MethodGet:
			return "list", "", 0
		case http.MethodPost:
			return "create", "", 0
		}
		return "sandboxes", "", http.StatusMethodNotAllowed
	case len(parts) == 2 && parts[0] == "sandboxes":
		id = parts[1]
		switch method {
		case http.MethodGet:
			return "get", id, 0
		case http.MethodDelete:
			return "delete", id, 0
		}
		return "sandbox", id, http.StatusMethodNotAllowed
	case len(parts) == 3 && parts[0] == "sandboxes":
		id = parts[1]
		switch parts[2] {
		case "start", "stop", "exec":
			return want(parts[2], http.MethodPost)
		}
	}
	return "unknown", "", http.StatusNotFound
}

func (h *tenantHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, id, status := parseRoute(r.Method, r.URL.Path)
	rc := &reqCtx{w: w, r: r, h: h, rec: Record{Project: h.t.Project, Method: r.Method, Route: route}}
	if v := r.Header.Get("X-S06-Req"); reqIDRE.MatchString(v) {
		rc.rec.ReqID = v
	}
	// A sandbox ID goes into the audit only in the form the server hands
	// out; anything else is the caller's text and stays out.
	if sandboxIDRE.MatchString(id) {
		rc.rec.Sandbox = id
	}
	p, err := peerFrom(r.Context())
	if err != nil {
		rc.deny(http.StatusForbidden, "peer credentials unavailable")
		return
	}
	rc.rec.PeerUID, rc.rec.PeerPID = p.UID, p.PID
	if p.UID != h.t.ControlRoomUID {
		h.s.log.Printf("project=%s: refused peer uid=%d pid=%d", h.t.Project, p.UID, p.PID)
		rc.deny(http.StatusForbidden, fmt.Sprintf("peer uid %d is not this project's control room", p.UID))
		return
	}
	if status != 0 {
		rc.deny(status, http.StatusText(status))
		return
	}
	switch route {
	case "whoami":
		h.whoami(rc, p)
	case "list":
		h.list(rc)
	case "create":
		h.create(rc)
	case "get":
		h.get(rc, id)
	case "delete":
		h.remove(rc, id)
	case "start", "stop":
		h.action(rc, id, route)
	case "exec":
		h.exec(rc, id)
	}
}

func (h *tenantHandler) whoami(rc *reqCtx, p Peer) {
	if !rc.allow() {
		return
	}
	writeJSON(rc.w, http.StatusOK, map[string]any{
		"project": h.t.Project, "peer_uid": p.UID, "peer_gid": p.GID, "peer_pid": p.PID,
		"peer_cgroup": peerCgroup(p.PID),
	})
}

func toTenant(sb Sandbox) TenantSandbox {
	return TenantSandbox{ID: sb.Slug, Status: sb.Status, CreatedAt: sb.CreatedAt}
}

// own returns the project's sandboxes that the tenant API handed out.
func (h *tenantHandler) own(ctx context.Context) ([]Sandbox, error) {
	all, err := h.s.admin.Sandboxes(ctx, h.t.Project)
	if err != nil {
		return nil, err
	}
	var out []Sandbox
	for _, sb := range all {
		if sandboxIDRE.MatchString(sb.Slug) {
			out = append(out, sb)
		}
	}
	return out, nil
}

// lookup finds one of the tenant's sandboxes. Another project's sandbox is
// never looked at, so it is indistinguishable from one that does not exist.
func (h *tenantHandler) lookup(ctx context.Context, id string) (Sandbox, bool, error) {
	if !sandboxIDRE.MatchString(id) {
		return Sandbox{}, false, nil
	}
	own, err := h.own(ctx)
	if err != nil {
		return Sandbox{}, false, err
	}
	for _, sb := range own {
		if sb.Slug == id {
			return sb, true, nil
		}
	}
	return Sandbox{}, false, nil
}

func (h *tenantHandler) list(rc *reqCtx) {
	own, err := h.own(rc.r.Context())
	if err != nil {
		rc.fail(http.StatusBadGateway, "management api unavailable")
		return
	}
	if !rc.allow() {
		return
	}
	out := make([]TenantSandbox, 0, len(own))
	for _, sb := range own {
		out = append(out, toTenant(sb))
	}
	writeJSON(rc.w, http.StatusOK, out)
}

func (h *tenantHandler) get(rc *reqCtx, id string) {
	sb, ok, err := h.lookup(rc.r.Context(), id)
	switch {
	case err != nil:
		rc.fail(http.StatusBadGateway, "management api unavailable")
	case !ok:
		rc.deny(http.StatusNotFound, noSuchSandbox)
	case rc.allow():
		writeJSON(rc.w, http.StatusOK, toTenant(sb))
	}
}

func (h *tenantHandler) action(rc *reqCtx, id, action string) {
	_, ok, err := h.lookup(rc.r.Context(), id)
	switch {
	case err != nil:
		rc.fail(http.StatusBadGateway, "management api unavailable")
	case !ok:
		rc.deny(http.StatusNotFound, noSuchSandbox)
	case rc.allow():
		if err := h.s.admin.SandboxAction(rc.r.Context(), h.t.Project, id, action); err != nil {
			h.s.log.Printf("project=%s sandbox=%s %s: %v", h.t.Project, id, action, err)
			writeJSON(rc.w, http.StatusBadGateway, apiError{action + " failed"})
			return
		}
		rc.w.WriteHeader(http.StatusNoContent)
	}
}

func (h *tenantHandler) remove(rc *reqCtx, id string) {
	_, ok, err := h.lookup(rc.r.Context(), id)
	switch {
	case err != nil:
		rc.fail(http.StatusBadGateway, "management api unavailable")
	case !ok:
		rc.deny(http.StatusNotFound, noSuchSandbox)
	case rc.allow():
		if err := h.s.admin.DeleteSandbox(rc.r.Context(), h.t.Project, id); err != nil {
			h.s.log.Printf("project=%s sandbox=%s delete: %v", h.t.Project, id, err)
			writeJSON(rc.w, http.StatusBadGateway, apiError{"delete failed"})
			return
		}
		rc.w.WriteHeader(http.StatusNoContent)
	}
}

// newSandboxID returns "t" and ten random lowercase letters and digits.
func newSandboxID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	out := []byte{'t'}
	for _, c := range b {
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out), nil
}

// checkCeiling compares a create request with the project's ceiling.
func checkCeiling(req createRequest, p Project) (int, string) {
	switch {
	case req.CPUMillis < 0 || req.MemBytes < 0 || req.PidsLimit < 0:
		return http.StatusBadRequest, "limits must not be negative"
	case req.Image != "" && req.Image != p.Image:
		return http.StatusForbidden, "image is not allowed for this project"
	case req.CPUMillis > p.CPUMillis:
		return http.StatusForbidden, "cpu_millis is above the ceiling"
	case req.MemBytes > p.MemBytes:
		return http.StatusForbidden, "mem_bytes is above the ceiling"
	case req.PidsLimit > p.PidsLimit:
		return http.StatusForbidden, "pids_limit is above the ceiling"
	}
	return 0, ""
}

func (h *tenantHandler) create(rc *reqCtx) {
	var req createRequest
	if err := decodeStrict(rc.r.Body, maxCreateBody, &req); err != nil {
		rc.deny(http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	ctx := rc.r.Context()
	h.mu.Lock()
	defer h.mu.Unlock()
	p, err := h.s.admin.Project(ctx, h.t.Project)
	if err != nil {
		rc.fail(http.StatusBadGateway, "management api unavailable")
		return
	}
	if status, reason := checkCeiling(req, p); status != 0 {
		rc.deny(status, reason)
		return
	}
	// The ceiling counts every sandbox of the project, also the owner's.
	all, err := h.s.admin.Sandboxes(ctx, h.t.Project)
	if err != nil {
		rc.fail(http.StatusBadGateway, "management api unavailable")
		return
	}
	if len(all) >= h.t.MaxSandboxes {
		rc.deny(http.StatusForbidden, "sandbox limit reached")
		return
	}
	id, err := newSandboxID()
	if err != nil {
		rc.fail(http.StatusInternalServerError, "no randomness")
		return
	}
	rc.rec.Sandbox = id
	if !rc.allow() {
		return
	}
	t0 := time.Now()
	sb, err := h.s.admin.CreateSandbox(ctx, h.t.Project, id)
	if err != nil {
		h.s.log.Printf("project=%s sandbox=%s create: %v", h.t.Project, id, err)
		writeJSON(rc.w, http.StatusBadGateway, apiError{"create failed"})
		return
	}
	if h.s.ready != nil {
		rctx, cancel := context.WithTimeout(ctx, readyTimeout)
		err := h.s.ready(rctx, p, sb)
		cancel()
		if err != nil {
			h.s.log.Printf("project=%s sandbox=%s not ready: %v", h.t.Project, id, err)
			dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			if derr := h.s.admin.DeleteSandbox(dctx, h.t.Project, id); derr != nil {
				h.s.log.Printf("project=%s sandbox=%s cleanup: %v", h.t.Project, id, derr)
			}
			dcancel()
			writeJSON(rc.w, http.StatusGatewayTimeout, apiError{"sandbox did not become ready"})
			return
		}
	}
	h.s.log.Printf("project=%s sandbox=%s created in %d ms", h.t.Project, id, time.Since(t0).Milliseconds())
	writeJSON(rc.w, http.StatusCreated, toTenant(sb))
}

// listenTenant opens the project's socket. The directory must exist with
// the layout the helper would create: owned by the server user, group the
// control room's primary group, mode 0750.
func listenTenant(t TenantConfig) (*net.UnixListener, error) {
	fi, err := os.Lstat(t.SocketDir)
	if err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("socket dir %s is not a directory", t.SocketDir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("socket dir: no stat")
	}
	if st.Uid != uint32(os.Getuid()) || fi.Mode().Perm() != 0o750 {
		return nil, fmt.Errorf("socket dir %s must be owned by uid %d with mode 0750", t.SocketDir, os.Getuid())
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(t.ControlRoomUID), 10))
	if err != nil {
		return nil, fmt.Errorf("control room uid %d: %w", t.ControlRoomUID, err)
	}
	if strconv.FormatUint(uint64(st.Gid), 10) != u.Gid {
		return nil, fmt.Errorf("socket dir %s must have group %s", t.SocketDir, u.Gid)
	}
	path := filepath.Join(t.SocketDir, "tenant.sock")
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(true)
	// The directory keeps others out; the server checks the peer UID.
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/s06/tenants.json", "configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	aud, err := OpenAudit(cfg.AuditPath)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "s06: ", log.LstdFlags|log.Lmicroseconds)
	s := NewServer(cfg, aud, NewAdminClient(cfg.AdminSocket), logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, len(cfg.Tenants))
	var servers []*http.Server
	for _, t := range cfg.Tenants {
		ln, err := listenTenant(t)
		if err != nil {
			return fmt.Errorf("tenant %s: %w", t.Project, err)
		}
		hs := &http.Server{
			Handler:           &tenantHandler{s: s, t: t},
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			ConnContext:       withPeer,
			ErrorLog:          logger,
		}
		servers = append(servers, hs)
		logger.Printf("project=%s listening on %s", t.Project, ln.Addr())
		go func() { errc <- hs.Serve(ln) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		logger.Printf("serve: %v", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, hs := range servers {
		_ = hs.Shutdown(sctx)
	}
	return nil
}
