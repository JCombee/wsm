package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Ops are the privileged operations behind the request types.
type Ops interface {
	CreateProjectUser(ctx context.Context, uid int) error
	DeleteProjectUser(ctx context.Context, uid int) error
	SetupWSM0(ctx context.Context) error
	ApplyBaseRules(ctx context.Context) error
	// PullServiceImage makes sure the image of a shared block for tag is
	// present for the service user.
	PullServiceImage(ctx context.Context, service, tag string) error
	// EnsureServiceInstance writes and starts the instance of a shared
	// block for tag on port, with password as its root password.
	EnsureServiceInstance(ctx context.Context, service, tag string, port int, password string) error
	// AllowService lets project user uid reach the service port on wsm0.
	AllowService(ctx context.Context, uid, port int) error
	// RevokeService takes that again; a missing grant is not an error.
	RevokeService(ctx context.Context, uid, port int) error
}

// Server answers requests from exactly one peer UID: the manager's.
type Server struct {
	Ops        Ops
	AllowedUID uint32 // the UID of user wsm
	Logf       func(format string, args ...any)
	// OpTimeout bounds one operation; zero means two minutes.
	OpTimeout time.Duration
	// LongOpTimeout bounds pulls and ensures of service instances; zero
	// means ten minutes.
	LongOpTimeout time.Duration

	mu sync.Mutex // serializes operations
}

// readTimeout bounds how long a client may take to send its request.
const readTimeout = 10 * time.Second

// ErrPeerRejected is returned for a connection from another UID.
var ErrPeerRejected = errors.New("peer is not the manager")

// PeerUID returns the UID of the process on the other end of conn, as the
// kernel reports it with SO_PEERCRED.
func PeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	if credErr != nil {
		return 0, fmt.Errorf("peer credentials: %w", credErr)
	}
	return cred.Uid, nil
}

// Serve accepts connections on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln *net.UnixListener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// ServeConn serves one connection and closes it.
func (s *Server) ServeConn(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()
	s.handle(ctx, conn)
}

// handle serves one connection. The peer check comes before anything is
// read, so another user cannot even make the server parse its input.
func (s *Server) handle(ctx context.Context, conn *net.UnixConn) {
	uid, err := PeerUID(conn)
	if err != nil {
		s.logf("rejected connection: %v", err)
		return
	}
	if uid != s.AllowedUID {
		s.logf("rejected connection from uid %d: %v", uid, ErrPeerRejected)
		writeResponse(conn, Response{Error: ErrPeerRejected.Error()})
		return
	}
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	req, err := DecodeRequest(conn)
	if err != nil {
		s.logf("invalid request: %v", err)
		writeResponse(conn, Response{Error: "invalid request: " + err.Error()})
		return
	}
	err = s.do(ctx, req)
	if err != nil {
		s.logf("%s %s: failed: %v", req.Type, describe(req), err)
		resp := Response{Error: err.Error()}
		if errors.Is(err, ErrNotFound) {
			resp.Code = CodeNotFound
		}
		writeResponse(conn, resp)
		return
	}
	s.logf("%s %s: ok", req.Type, describe(req))
	writeResponse(conn, Response{OK: true})
}

// describe names the fields of a request for the log: never the password.
func describe(r Request) string {
	var b strings.Builder
	if r.UID != nil {
		fmt.Fprintf(&b, " uid=%d", *r.UID)
	}
	if r.Service != "" {
		fmt.Fprintf(&b, " service=%s", r.Service)
	}
	if r.Tag != "" {
		fmt.Fprintf(&b, " tag=%s", r.Tag)
	}
	if r.Port != nil {
		fmt.Fprintf(&b, " port=%d", *r.Port)
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()[1:]
}

// timeout returns how long an operation of type typ may take.
func (s *Server) timeout(typ string) time.Duration {
	if typ == TypePullServiceImage || typ == TypeEnsureServiceInstance {
		if s.LongOpTimeout != 0 {
			return s.LongOpTimeout
		}
		return 10 * time.Minute
	}
	if s.OpTimeout != 0 {
		return s.OpTimeout
	}
	return 2 * time.Minute
}

func (s *Server) do(ctx context.Context, req Request) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout(req.Type))
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Validate again: do must never run an unvalidated request.
	if err := req.Validate(); err != nil {
		return err
	}
	switch req.Type {
	case TypeCreateProjectUser:
		return s.Ops.CreateProjectUser(ctx, *req.UID)
	case TypeDeleteProjectUser:
		return s.Ops.DeleteProjectUser(ctx, *req.UID)
	case TypeSetupWSM0:
		return s.Ops.SetupWSM0(ctx)
	case TypeApplyBaseRules:
		return s.Ops.ApplyBaseRules(ctx)
	case TypePullServiceImage:
		return s.Ops.PullServiceImage(ctx, req.Service, req.Tag)
	case TypeEnsureServiceInstance:
		return s.Ops.EnsureServiceInstance(ctx, req.Service, req.Tag, *req.Port, req.Password)
	case TypeAllowService:
		return s.Ops.AllowService(ctx, *req.UID, *req.Port)
	case TypeRevokeService:
		return s.Ops.RevokeService(ctx, *req.UID, *req.Port)
	}
	return fmt.Errorf("unknown request type %q", req.Type)
}

func writeResponse(conn *net.UnixConn, r Response) {
	if len(r.Error) > MaxMessage/2 {
		r.Error = r.Error[:MaxMessage/2] + "..."
	}
	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	conn.SetWriteDeadline(time.Now().Add(readTimeout))
	if _, err := conn.Write(data); err != nil {
		return
	}
	// Closing with unread input makes the kernel reset the connection, and
	// the peer would lose the response. Half-close, then discard a bounded
	// amount of what is left, unparsed.
	conn.CloseWrite()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	io.Copy(io.Discard, io.LimitReader(conn, 4*MaxMessage))
}
