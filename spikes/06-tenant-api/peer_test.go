package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPeerCred(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if errors.Is(err, syscall.EPERM) {
		t.Skip("this environment forbids unix sockets; the VM run covers the peer check")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- nil
			return
		}
		done <- c
	}()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-done
	if server == nil {
		t.Fatal("accept failed")
	}
	defer server.Close()
	p, err := peerCred(server)
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != uint32(os.Getuid()) || p.PID != int32(os.Getpid()) {
		t.Fatalf("peerCred = %+v, want uid %d pid %d", p, os.Getuid(), os.Getpid())
	}
}
