package broker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Limits for reading a greeting, which is untrusted input (CLAUDE.md
// invariant 8).
const (
	maxGreeting     = 1024
	greetingTimeout = 2 * time.Second
	// mysqlProtocol is the protocol version byte of a MariaDB/MySQL
	// handshake packet.
	mysqlProtocol = 0x0a
	// compatPrefix is what MariaDB 10.x put before its version for old
	// MySQL clients.
	compatPrefix = "5.5.5-"
)

// readGreeting reads the initial handshake packet of a MariaDB server from
// conn and returns its server version: at most maxGreeting bytes, within
// greetingTimeout.
func readGreeting(conn net.Conn) (string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(greetingTimeout)); err != nil {
		return "", fmt.Errorf("greeting: %w", err)
	}
	r := io.LimitReader(conn, maxGreeting)
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", fmt.Errorf("greeting header: %w", err)
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if n == 0 || n > maxGreeting-len(hdr) {
		return "", fmt.Errorf("greeting of %d bytes", n)
	}
	if hdr[3] != 0 {
		return "", fmt.Errorf("greeting has sequence %d", hdr[3])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", fmt.Errorf("greeting: %w", err)
	}
	if payload[0] != mysqlProtocol {
		return "", fmt.Errorf("greeting has protocol %#x, want %#x", payload[0], mysqlProtocol)
	}
	end := bytes.IndexByte(payload[1:], 0)
	if end < 0 {
		return "", errors.New("greeting has no terminated server version")
	}
	v := string(payload[1 : 1+end])
	if v == "" {
		return "", errors.New("greeting has an empty server version")
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return "", errors.New("greeting has a server version with unprintable characters")
		}
	}
	return v, nil
}

// versionMatches reports whether a server version belongs to tag: after an
// optional "5.5.5-", it equals the tag or continues it with "." or "-".
// So 11.8.9-MariaDB matches 11.8 and 11.8.9, but 11.80.1 does not match 11.8.
func versionMatches(version, tag string) bool {
	v := strings.TrimPrefix(version, compatPrefix)
	return v == tag || strings.HasPrefix(v, tag+".") || strings.HasPrefix(v, tag+"-")
}
