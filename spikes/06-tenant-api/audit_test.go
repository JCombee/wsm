package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newAuditFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAuditChain(t *testing.T) {
	p := newAuditFile(t)
	a, err := OpenAudit(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := a.Write(Record{Project: "a", Route: "whoami", Decision: "allow", ReqID: strings.Repeat("x", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(p)
	if n, err := VerifyAudit(bytes.NewReader(data)); err != nil || n != 3 {
		t.Fatalf("VerifyAudit = %d, %v; want 3, nil", n, err)
	}

	// Reopening continues the chain.
	a2, err := OpenAudit(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := a2.Write(Record{Project: "a", Route: "list", Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p)
	if n, err := VerifyAudit(bytes.NewReader(data)); err != nil || n != 4 {
		t.Fatalf("after reopen: VerifyAudit = %d, %v; want 4, nil", n, err)
	}

	// Changing one byte of line 2 breaks the prev of line 3.
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	lines[1] = bytes.Replace(lines[1], []byte(`"route":"whoami"`), []byte(`"route":"whoamI"`), 1)
	tampered := append(bytes.Join(lines, []byte("\n")), '\n')
	if _, err := VerifyAudit(bytes.NewReader(tampered)); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("tampered: VerifyAudit = %v, want an error naming line 3", err)
	}
}

func TestAuditFailsClosed(t *testing.T) {
	p := newAuditFile(t)
	a, err := OpenAudit(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := a.Write(Record{Decision: "allow"}); err == nil {
		t.Fatal("Write to a removed file succeeded; it must not create a fresh chain")
	}
	if _, err := OpenAudit(p); err == nil {
		t.Fatal("OpenAudit of a missing file succeeded")
	}
}
