package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Peer is the process on the other end of a Unix connection, as the kernel
// reported it with SO_PEERCRED at connect time. The kernel translates the
// IDs into the server's namespaces, so a process in a rootless container
// shows up with its host UID and host PID.
type Peer struct {
	UID uint32
	GID uint32
	PID int32
}

// peerCred returns the credentials of the process that opened c.
func peerCred(c net.Conn) (Peer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return Peer{}, errors.New("peer credentials: not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, fmt.Errorf("peer credentials: %w", err)
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return Peer{}, fmt.Errorf("peer credentials: %w", err)
	}
	if credErr != nil {
		return Peer{}, fmt.Errorf("peer credentials: %w", credErr)
	}
	return Peer{UID: cred.Uid, GID: cred.Gid, PID: cred.Pid}, nil
}

type peerKey struct{}

type peerResult struct {
	peer Peer
	err  error
}

// withPeer is the http.Server ConnContext hook: it reads the peer
// credentials once per connection.
func withPeer(ctx context.Context, c net.Conn) context.Context {
	p, err := peerCred(c)
	return context.WithValue(ctx, peerKey{}, peerResult{p, err})
}

func peerFrom(ctx context.Context) (Peer, error) {
	v, ok := ctx.Value(peerKey{}).(peerResult)
	if !ok {
		return Peer{}, errors.New("peer credentials: missing")
	}
	return v.peer, v.err
}

// peerCgroup returns the first line of /proc/<pid>/cgroup, for the
// findings: it shows which container a peer runs in.
func peerCgroup(pid int32) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(int(pid)) + "/cgroup")
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	if len(line) > 512 {
		line = line[:512]
	}
	return line
}
