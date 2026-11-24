package main

import (
	"errors"
	"log"
	"net/http"
)

// Execer runs commands in a sandbox's work container. Step 2 implements it
// over the project's Podman socket.
type Execer interface{}

// NewServer returns a server without an exec runner yet.
func NewServer(cfg Config, aud *Audit, admin Admin, logger *log.Logger) *Server {
	return &Server{cfg: cfg, audit: aud, admin: admin, log: logger}
}

func (h *tenantHandler) exec(rc *reqCtx, _ string) {
	rc.deny(http.StatusNotImplemented, "exec is not built yet")
}

func runClient([]string) error { return errors.New("client is not built yet") }
