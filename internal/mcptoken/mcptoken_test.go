package mcptoken

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestNew(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, hash, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != Length || !ValidToken(tok) {
			t.Fatalf("token has the wrong form (length %d)", len(tok))
		}
		sum := sha256.Sum256([]byte(tok))
		if hash != hex.EncodeToString(sum[:]) || !ValidHash(hash) {
			t.Fatal("hash is not the sha256 of the token as lowercase hex")
		}
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		s           string
		token, hash bool
	}{
		{"", false, false},
		{"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO-_", true, false},
		{"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO-=", false, false},
		{"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO-", false, false},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", false, true},
		{"0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef", false, false},
	}
	for _, tt := range tests {
		if got := ValidToken(tt.s); got != tt.token {
			t.Errorf("ValidToken(%q) = %v", tt.s, got)
		}
		if got := ValidHash(tt.s); got != tt.hash {
			t.Errorf("ValidHash(%q) = %v", tt.s, got)
		}
	}
}

func TestURL(t *testing.T) {
	if got, want := URL("abcdefgh2345"), "http://10.255.0.1:7443/mcp/abcdefgh2345"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if got := Addr(); got != "10.255.0.1:7443" {
		t.Errorf("Addr = %q", got)
	}
}
