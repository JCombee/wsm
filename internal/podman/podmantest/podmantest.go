// Package podmantest serves a fake Podman API over in-memory pipes, so
// tests need no socket files.
package podmantest

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/JCombee/wsm/internal/podman"
)

// pipeListener is a net.Listener whose connections are in-memory pipes.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Server is a fake Podman API.
type Server struct {
	ln  *pipeListener
	srv *http.Server
}

// New serves h until the test ends.
func New(t testing.TB, h http.Handler) *Server {
	t.Helper()
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &Server{ln: ln, srv: srv}
}

// Client returns a new client for the fake API.
func (s *Server) Client() *podman.Client { return podman.NewClientDialer(s.ln.dial) }
