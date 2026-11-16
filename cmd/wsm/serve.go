package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"time"

	"github.com/JCombee/wsm/internal/api"
	"github.com/JCombee/wsm/internal/audit"
	"github.com/JCombee/wsm/internal/broker"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/gitmgr"
	"github.com/JCombee/wsm/internal/helper"
	"github.com/JCombee/wsm/internal/mcp"
	"github.com/JCombee/wsm/internal/netpolicy"
	"github.com/JCombee/wsm/internal/podman"
	"github.com/JCombee/wsm/internal/sandbox"
	"github.com/JCombee/wsm/internal/sdsocket"
	"github.com/JCombee/wsm/internal/secret"
)

// runServe implements "wsm serve": the manager, running as user wsm.
func runServe(e env) error {
	fs := newFlags("serve")
	dbPath := fs.String("db", defaultDB, "path to the manager database")
	socket := fs.String("socket", api.DefaultSocket, "API socket, unless systemd passes one")
	helperSocket := fs.String("helper-socket", helper.DefaultSocket, "wsm-helper socket")
	keyFile := fs.String("secret-key", secret.DefaultKeyFile, "key file that seals deploy keys; created on first start")
	if err := fs.Parse(e.args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() != 0 {
		return usageError{"unexpected arguments"}
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
	mgr := &sandbox.Manager{
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

	apiSrv := &http.Server{
		Handler: (&api.Server{Manager: mgr, Broker: brk, UserName: helper.UserName,
			Logf: logger.Printf}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          logger,
	}
	// The MCP server listens only on wsm0, which SetupWSM0 created above
	// (ADR 0012). Without it there is no MCP, so wsm does not start.
	mcpSrv := mcp.NewHTTPServer((&mcp.Server{
		Store: store, Audit: audit.New(sqlDB), Services: brk, Logf: logger.Printf, Version: version,
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
	errc := make(chan error, 2)
	go func() { errc <- fmt.Errorf("serve api: %w", apiSrv.Serve(ln)) }()
	go func() { errc <- fmt.Errorf("serve mcp: %w", mcpSrv.Serve(mcpLn)) }()
	logger.Printf("wsm %s serving the api on %s and mcp on %s", version, ln.Addr(), mcpLn.Addr())

	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var shutdownErr error
	for _, srv := range []*http.Server{apiSrv, mcpSrv} {
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
