package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/JCombee/wsm/internal/api"
	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/gitmgr"
	"github.com/JCombee/wsm/internal/helper"
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

	mgr := &sandbox.Manager{
		Store: db.NewStore(sqlDB),
		Users: hc,
		Podman: func(uid int) *podman.Client {
			return podman.NewClient(helper.PodmanSocket(uid))
		},
		Secrets: box,
		Mirrors: mirrors,
		Logf:    logger.Printf,
	}
	srv := &http.Server{
		Handler:           (&api.Server{Manager: mgr, UserName: helper.UserName, Logf: logger.Printf}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          logger,
	}
	ln, err := sdsocket.Listen(*socket, 0o660)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Printf("wsm %s serving on %s", version, ln.Addr())

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	logger.Printf("wsm stopped")
	return nil
}
