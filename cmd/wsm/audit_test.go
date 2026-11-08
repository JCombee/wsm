package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JCombee/wsm/internal/audit"
	"github.com/JCombee/wsm/internal/db"
)

func TestAuditVerifyCommand(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wsm.db")
	sqlDB, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	l := audit.New(sqlDB)
	for _, tool := range []string{"sandbox_info", "service_enable"} {
		if _, err := l.Append(ctx, audit.Event{Actor: "test", Tool: tool, Result: audit.ResultOK}); err != nil {
			t.Fatal(err)
		}
	}

	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"audit", "verify", "--db", path}, nil, &out, &errOut); code != 0 {
		t.Fatalf("intact chain: code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "OK: 2 events") {
		t.Errorf("stdout = %q", out.String())
	}

	for _, s := range []string{
		`DROP TRIGGER audit_events_no_update`,
		`UPDATE audit_events SET detail = 'tampered' WHERE seq = 1`,
	} {
		if _, err := sqlDB.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	sqlDB.Close()
	out.Reset()
	if code := run(ctx, []string{"audit", "verify", "--db", path}, nil, &out, &errOut); code != 1 {
		t.Fatalf("tampered chain: code %d, stdout %q", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL: audit chain broken at event 1") {
		t.Errorf("stdout = %q", out.String())
	}

	if code := run(ctx, []string{"audit", "verify", "--db", filepath.Join(t.TempDir(), "missing.db")}, nil, &out, &errOut); code != 1 {
		t.Errorf("missing database: code %d", code)
	}
	if code := run(ctx, []string{"audit"}, nil, &out, &errOut); code != 2 {
		t.Errorf("no subcommand: code %d", code)
	}
}
