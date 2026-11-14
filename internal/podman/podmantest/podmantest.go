// Package podmantest serves a fake Podman API on a loopback TCP listener,
// so tests need no socket files. TCP, like the real unix socket, can
// half-close a connection, which exec with stdin needs.
package podmantest

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/JCombee/wsm/internal/podman"
)

// Server is a fake Podman API.
type Server struct {
	addr string
}

// New serves h until the test ends.
func New(t testing.TB, h http.Handler) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("podmantest: %v", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &Server{addr: ln.Addr().String()}
}

// Client returns a new client for the fake API.
func (s *Server) Client() *podman.Client {
	return podman.NewClientDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", s.addr)
	})
}
