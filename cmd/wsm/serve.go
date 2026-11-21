package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/JCombee/wsm/internal/api"
	"github.com/JCombee/wsm/internal/audit"
	"github.com/JCombee/wsm/internal/auth"
	"github.com/JCombee/wsm/internal/broker"
	"github.com/JCombee/wsm/internal/dashboard"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/gitmgr"
	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/mcp"
	"github.com/JCombee/wsm/internal/netpolicy"
	"github.com/JCombee/wsm/internal/podman"
	"github.com/JCombee/wsm/internal/preview"
	"github.com/JCombee/wsm/internal/sandbox"
	"github.com/JCombee/wsm/internal/sdsocket"
	"github.com/JCombee/wsm/internal/secret"
	"github.com/JCombee/wsm/web"
)

// runServe implements "wsm serve": the manager, running as user wsm.
func runServe(e env) error {
	fs := newFlags("serve")
	dbPath := fs.String("db", defaultDB, "path to the manager database")
	socket := fs.String("socket", api.DefaultSocket, "API socket, unless systemd passes one")
	helperSocket := fs.String("helper-socket", helper.DefaultSocket, "wsm-helper socket")
	keyFile := fs.String("secret-key", secret.DefaultKeyFile, "key file that seals deploy keys; created on first start")
	dashHost := fs.String("dashboard-host", "", "host name of the dashboard, e.g. dash.example.com; empty: no dashboard")
	dashAddr := fs.String("dashboard-addr", "127.0.0.1:8080", "loopback address the dashboard listens on, behind a TLS proxy")
	previewDomain := fs.String("preview-domain", "", "domain of preview hosts, e.g. preview.example.net; empty: no previews")
	previewAddr := fs.String("preview-addr", "127.0.0.1:8081", "loopback address the preview proxy listens on, behind a TLS proxy")
	helperDir := fs.String("helper-dir", sandbox.DefaultHelperDir, "host directory with wsm-fs, mounted read-only into sandboxes")
	if err := fs.Parse(e.args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() != 0 {
		return usageError{"unexpected arguments"}
	}
	if *previewDomain != "" && *dashHost == "" {
		return usageError{"--preview-domain needs --dashboard-host"}
	}
	logger := log.New(e.stderr, "", 0)
	ctx := e.ctx

	sqlDB, err := db.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	// The key file is created on the first start (0600, in the state
	// directory) and must keep that mode and size afterwards.
	key, err := secret.LoadOrCreateKey(*keyFile)
	if err != nil {
		return err
	}
	box, err := secret.New(key)
	clear(key)
	if err != nil {
		return err
	}

	mirrors, err := gitmgr.NewMirrors(gitmgr.DefaultDir, gitmgr.DefaultHome, gitmgr.DefaultGit)
	if err != nil {
		return err
	}

	hc := &helper.Client{Socket: *helperSocket}
	// Sandboxes must never run without the egress rules, so the manager
	// does not start without them.
	if err := hc.SetupWSM0(ctx); err != nil {
		return fmt.Errorf("set up wsm0: %w", err)
	}
	if err := hc.ApplyBaseRules(ctx); err != nil {
		return fmt.Errorf("load base rules: %w", err)
	}

	store := db.NewStore(sqlDB)
	host := sandbox.Host{PreviewDomain: *previewDomain, HelperDir: *helperDir}
	// A bind mount of a missing directory would make every sandbox fail.
	if _, err := os.Stat(filepath.Join(*helperDir, "wsm-fs")); err != nil {
		logger.Printf("file helper: %v; sandboxes get no file access from the dashboard", err)
		host.HelperDir = ""
	}
	mgr := &sandbox.Manager{
		Host:  host,
		Store: store,
		Users: hc,
		Podman: func(uid int) *podman.Client {
			return podman.NewClient(helper.PodmanSocket(uid))
		},
		Secrets: box,
		Mirrors: mirrors,
		Logf:    logger.Printf,
	}
	brk := &broker.Broker{
		Store:   store,
		Helper:  hc,
		Secrets: box,
		Addr:    netip.MustParseAddr(netpolicy.DefaultWSM0Addr),
		Logf:    logger.Printf,
	}
	mgr.Grants = brk
	brk.Pods = mgr
	// ApplyBaseRules emptied svc_allow4: put the elements of the existing
	// grants back before the API and the MCP open. Failures are logged and
	// not fatal; access then stays closed (ADR 0014).
	if n, err := brk.RestoreGrants(ctx); err != nil {
		logger.Printf("restore service grants: %v", err)
	} else if n > 0 {
		logger.Printf("restore service grants: %d failed", n)
	}
	// Bring every shared service instance back in line with its template,
	// in the background: the API and the MCP do not wait for it (ADR 0013).
	go brk.EnsureAll(ctx)

	auditLog := audit.New(sqlDB)
	apiServer := &api.Server{Manager: mgr, Broker: brk, UserName: helper.UserName, Audit: auditLog,
		Logf: logger.Printf}
	var authSvc *auth.Service
	if *dashHost != "" {
		authSvc, err = auth.New(store, auth.Config{DashboardHost: *dashHost, PreviewDomain: *previewDomain}, logger.Printf)
		if err != nil {
			return usageError{err.Error()}
		}
		apiServer.Enroll = authSvc.NewEnrollToken
		go authSvc.Cleanup(ctx, time.Hour)
	}
	apiHandler := apiServer.Handler()
	apiSrv := &http.Server{
		Handler:           apiHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          logger,
	}
	// The MCP server listens only on wsm0, which SetupWSM0 created above
	// (ADR 0012). Without it there is no MCP, so wsm does not start.
	mcpSrv := mcp.NewHTTPServer((&mcp.Server{
		Store: store, Audit: auditLog, Services: brk, Logf: logger.Printf, Version: version,
	}).Handler(), logger)
	mcpLn, err := mcp.Listen(ctx)
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	ln, err := sdsocket.Listen(*socket, 0o660)
	if err != nil {
		mcpLn.Close()
		return err
	}
	servers := []*http.Server{apiSrv, mcpSrv}
	errc := make(chan error, 4)
	go func() { errc <- fmt.Errorf("serve api: %w", apiSrv.Serve(ln)) }()
	go func() { errc <- fmt.Errorf("serve mcp: %w", mcpSrv.Serve(mcpLn)) }()
	logger.Printf("wsm %s serving the api on %s and mcp on %s", version, ln.Addr(), mcpLn.Addr())
	if authSvc != nil {
		dashSrv := &http.Server{
			Handler:           dashboard.Handler(authSvc, apiHandler, web.Dist(), *dashHost),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
			ErrorLog:          logger,
		}
		if err := serveTCP(ctx, dashSrv, *dashAddr, errc, "dashboard"); err != nil {
			return err
		}
		servers = append(servers, dashSrv)
		logger.Printf("dashboard for https://%s on %s", *dashHost, *dashAddr)
	}
	if authSvc != nil && *previewDomain != "" {
		prx := preview.New(authSvc, previewTarget(store), logger.Printf)
		prvSrv := &http.Server{
			Handler:           prx,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    preview.MaxHeaderBytes,
			ErrorLog:          logger,
		}
		if err := serveTCP(ctx, prvSrv, *previewAddr, errc, "preview"); err != nil {
			return err
		}
		servers = append(servers, prvSrv)
		logger.Printf("previews for *.%s on %s", *previewDomain, *previewAddr)
	}

	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var shutdownErr error
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) && shutdownErr == nil {
			shutdownErr = err
		}
	}
	if serveErr != nil {
		return serveErr
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	logger.Printf("wsm stopped")
	return nil
}

// serveTCP serves srv on a loopback TCP address. Only loopback is allowed:
// TLS and the public side belong to the proxy in front (plan section 7).
func serveTCP(ctx context.Context, srv *http.Server, addr string, errc chan<- error, name string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return usageError{fmt.Sprintf("%s address %q: %v", name, addr, err)}
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
		return usageError{fmt.Sprintf("%s address %q: use a loopback address", name, addr)}
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	go func() { errc <- fmt.Errorf("serve %s: %w", name, srv.Serve(ln)) }()
	return nil
}

// previewTarget finds the published port of a sandbox for the preview
// proxy.
func previewTarget(store *db.Store) preview.Target {
	return func(ctx context.Context, projectSlug, sandboxSlug string) (int, bool, error) {
		p, err := store.ProjectBySlug(ctx, projectSlug)
		if err != nil {
			return 0, false, err
		}
		sb, err := store.SandboxBySlug(ctx, p.ID, sandboxSlug)
		if err != nil {
			return 0, false, err
		}
		if p.AppPort == 0 || sb.HostPort == 0 {
			return 0, false, db.ErrNotFound
		}
		return sb.HostPort, sb.Status == db.StatusRunning, nil
	}
}
