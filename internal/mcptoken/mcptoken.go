// Package mcptoken creates and hashes the per-sandbox MCP bearer tokens
// (ADR 0012). A token is 32 random bytes as unpadded base64url, 43
// characters. The manager stores only its hash: the SHA-256 of those 43
// characters as lowercase hex.
package mcptoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strconv"

	"github.com/JCombee/wsm/internal/netpolicy"
)

// Length is the length of a token in characters.
const Length = 43

// tokenBytes is the number of random bytes in a token.
const tokenBytes = 32

var (
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	hashPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// New returns a fresh token and its hash.
func New() (token, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate mcp token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	clear(b)
	return token, Hash(token), nil
}

// Hash returns the stored form of a token.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ValidToken reports whether s has the form New produces.
func ValidToken(s string) bool { return tokenPattern.MatchString(s) }

// ValidHash reports whether s has the form Hash produces.
func ValidHash(s string) bool { return hashPattern.MatchString(s) }

// Addr is the address the MCP server listens on: wsm0's address and the
// MCP port from the base ruleset.
func Addr() string {
	return net.JoinHostPort(netpolicy.DefaultWSM0Addr, strconv.Itoa(netpolicy.DefaultMCPPort))
}

// PathPrefix is the path below which every sandbox has its own endpoint.
const PathPrefix = "/mcp/"

// URL returns the MCP endpoint of a sandbox, as the workspace container
// sees it in WSM_MCP_URL.
func URL(sandboxID string) string {
	return "http://" + Addr() + PathPrefix + sandboxID
}
