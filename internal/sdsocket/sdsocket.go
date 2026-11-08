// Package sdsocket picks up a unix socket that systemd opened for the
// process (socket activation), or opens one itself.
//
// With socket activation the owner, group and mode of the socket file come
// from the .socket unit, so the daemon never has to chown a path in a
// directory another user can write to.
package sdsocket

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

// firstFD is the first file descriptor systemd passes (SD_LISTEN_FDS_START).
const firstFD = 3

// Listen returns the one socket systemd passed, when LISTEN_PID names this
// process. Otherwise it removes a stale socket at path and listens there
// with the given mode.
func Listen(path string, mode os.FileMode) (*net.UnixListener, error) {
	ln, err := fromSystemd()
	if err != nil || ln != nil {
		return ln, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("listen %s: exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("listen %s: %w", path, err)
		}
	}
	ln, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	return ln, nil
}

func fromSystemd() (*net.UnixListener, error) {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n != 1 {
		return nil, fmt.Errorf("socket activation: want exactly 1 socket, got LISTEN_FDS=%q", os.Getenv("LISTEN_FDS"))
	}
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_FDNAMES")
	f := os.NewFile(firstFD, "systemd-socket")
	defer f.Close()
	l, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("socket activation: %w", err)
	}
	ul, ok := l.(*net.UnixListener)
	if !ok {
		l.Close()
		return nil, errors.New("socket activation: passed socket is not a unix socket")
	}
	return ul, nil
}
