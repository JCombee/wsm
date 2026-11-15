package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestSandboxTokenHash(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := newProject("proj")
	if err := s.CreateProject(ctx, p, userName); err != nil {
		t.Fatal(err)
	}
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO-_"
	hash := hashOf(token)
	sb := &Sandbox{ProjectID: p.ID, Slug: "a", MCPTokenHash: hash}
	if err := s.CreateSandbox(ctx, sb, podName); err != nil {
		t.Fatal(err)
	}

	// Only the hash is in the row; the token appears nowhere in it.
	var stored string
	var row []any
	rows, err := s.DB.QueryContext(ctx, `SELECT * FROM sandboxes WHERE id = ?`, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	for rows.Next() {
		row = make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()
	for i, v := range row {
		var str string
		switch v := v.(type) {
		case string:
			str = v
		case []byte:
			str = string(v)
		}
		if strings.Contains(str, token) {
			t.Errorf("column %s contains the token", cols[i])
		}
		if cols[i] == "mcp_token_hash" {
			stored = str
		}
	}
	if stored != hash {
		t.Errorf("mcp_token_hash = %q, want the sha256 of the token", stored)
	}

	gotP, gotSB, err := s.SandboxByTokenHash(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if gotP.ID != p.ID || gotP.Slug != "proj" || gotSB.ID != sb.ID || gotSB.MCPTokenHash != hash {
		t.Errorf("lookup = %+v, %+v", gotP, gotSB)
	}

	// Two sandboxes cannot share a hash.
	if err := s.CreateSandbox(ctx, &Sandbox{ProjectID: p.ID, Slug: "b", MCPTokenHash: hash}, podName); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate hash: err = %v, want ErrConflict", err)
	}
	// Sandboxes without a token (from before migration 00005) can coexist.
	for _, slug := range []string{"old1", "old2"} {
		if err := s.CreateSandbox(ctx, &Sandbox{ProjectID: p.ID, Slug: slug}, podName); err != nil {
			t.Errorf("sandbox without a token: %v", err)
		}
	}

	// The CHECK constraint rejects anything but 64 lowercase hex digits.
	for i, bad := range []string{token, strings.ToUpper(hash), hash[:63], hash + "0", strings.Repeat("g", 64)} {
		err := s.CreateSandbox(ctx, &Sandbox{ProjectID: p.ID, Slug: "bad" + string(rune('a'+i)), MCPTokenHash: bad}, podName)
		if err == nil {
			t.Errorf("hash %d was accepted", i)
		}
	}

	for name, h := range map[string]string{
		"unknown hash": hashOf("other"),
		"empty hash":   "",
	} {
		if _, _, err := s.SandboxByTokenHash(ctx, h); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}

	if err := s.DeleteSandbox(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SandboxByTokenHash(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted sandbox: err = %v, want ErrNotFound", err)
	}
}
