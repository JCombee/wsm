package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JCombee/wsm/internal/db"
)

func openLog(t *testing.T) *Log {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "wsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	l := New(sqlDB)
	tick := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { tick = tick.Add(time.Second); return tick }
	return l
}

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := range n {
		_, err := l.Append(context.Background(), Event{
			Actor: "mcp:abcdefghijkl", SandboxID: "abcdefghijkl", Tool: "service_enable",
			Args:   json.RawMessage(`{"block":"mariadb","n":` + string(rune('0'+i)) + `}`),
			Result: ResultOK,
		})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

func TestAppendOnly(t *testing.T) {
	ctx := context.Background()
	l := openLog(t)
	appendN(t, l, 3)

	for _, stmt := range []string{
		`UPDATE audit_events SET detail = 'changed' WHERE seq = 2`,
		`UPDATE audit_events SET hash = 'x'`,
		`DELETE FROM audit_events WHERE seq = 3`,
		`DELETE FROM audit_events`,
	} {
		_, err := l.DB.ExecContext(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only error", stmt, err)
		}
	}
	n, err := l.Verify(ctx)
	if err != nil || n != 3 {
		t.Fatalf("Verify = %d, %v; want 3, nil", n, err)
	}
}

func TestChainLinks(t *testing.T) {
	ctx := context.Background()
	l := openLog(t)
	first, err := l.Append(ctx, Event{Actor: "cli", Tool: "a", Result: ResultOK})
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.Append(ctx, Event{Actor: "cli", Tool: "b", Result: ResultDenied, Detail: "outside the ceiling"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || first.PrevHash != GenesisHash || first.ArgsJSON != "{}" {
		t.Errorf("first = %+v", first)
	}
	if second.Seq != 2 || second.PrevHash != first.Hash || second.Hash == first.Hash {
		t.Errorf("second = %+v", second)
	}
	if n, err := l.Verify(ctx); err != nil || n != 2 {
		t.Errorf("Verify = %d, %v", n, err)
	}
}

func TestAppendRejectsInvalidEvents(t *testing.T) {
	l := openLog(t)
	for name, e := range map[string]Event{
		"bad result":    {Actor: "cli", Tool: "x", Result: "maybe"},
		"no actor":      {Tool: "x", Result: ResultOK},
		"no tool":       {Actor: "cli", Result: ResultOK},
		"invalid json":  {Actor: "cli", Tool: "x", Result: ResultOK, Args: json.RawMessage(`{"a":`)},
		"trailing json": {Actor: "cli", Tool: "x", Result: ResultOK, Args: json.RawMessage(`{} {}`)},
		"too large":     {Actor: "cli", Tool: "x", Result: ResultOK, Args: json.RawMessage(`"` + strings.Repeat("a", maxArgs) + `"`)},
	} {
		if _, err := l.Append(context.Background(), e); err == nil {
			t.Errorf("%s: Append accepted the event", name)
		}
	}
}

// tamper drops the append-only triggers, as someone editing the database
// file directly could, and runs stmt.
func tamper(t *testing.T, sqlDB *sql.DB, stmt string) {
	t.Helper()
	for _, s := range []string{
		`DROP TRIGGER audit_events_no_update`,
		`DROP TRIGGER audit_events_no_delete`,
		stmt,
	} {
		if _, err := sqlDB.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	tests := []struct {
		name    string
		stmt    string
		wantSeq int64
	}{
		{"changed detail", `UPDATE audit_events SET detail = 'nothing to see' WHERE seq = 2`, 2},
		{"changed args", `UPDATE audit_events SET args_json = '{"block":"redis"}' WHERE seq = 3`, 3},
		{"changed result", `UPDATE audit_events SET result = 'ok', detail = '' WHERE seq = 4`, 4},
		{"changed timestamp", `UPDATE audit_events SET ts = '2020-01-01T00:00:00.000000000Z' WHERE seq = 1`, 1},
		{"changed actor", `UPDATE audit_events SET actor = 'cli' WHERE seq = 5`, 5},
		{"deleted event", `DELETE FROM audit_events WHERE seq = 3`, 4},
		{"renumbered events", `UPDATE audit_events SET seq = seq + 10 WHERE seq >= 4`, 14},
		{"recomputed own hash", `UPDATE audit_events SET detail = 'x', hash = '` + strings.Repeat("a", 64) + `' WHERE seq = 2`, 2},
		{"forged prev_hash", `UPDATE audit_events SET prev_hash = '` + strings.Repeat("b", 64) + `' WHERE seq = 3`, 3},
		{"invalid args json", `UPDATE audit_events SET args_json = '{' WHERE seq = 2`, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := openLog(t)
			appendN(t, l, 3)
			if _, err := l.Append(context.Background(), Event{Actor: "mcp:abcdefghijkl", Tool: "service_enable",
				Result: ResultDenied, Detail: "postgres is outside the ceiling"}); err != nil {
				t.Fatal(err)
			}
			appendN(t, l, 2)
			if _, err := l.Verify(context.Background()); err != nil {
				t.Fatalf("Verify before tampering: %v", err)
			}
			tamper(t, l.DB, tc.stmt)
			_, err := l.Verify(context.Background())
			var ve *VerifyError
			if !errors.As(err, &ve) {
				t.Fatalf("Verify = %v, want *VerifyError", err)
			}
			if ve.Seq != tc.wantSeq {
				t.Errorf("broken at %d (%s), want %d", ve.Seq, ve.Reason, tc.wantSeq)
			}
		})
	}
}

func TestRedaction(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		detail     string
		wantArgs   string
		wantDetail string
	}{
		{
			name:     "sensitive keys at any depth",
			args:     `{"user":"sbx_a","password":"hunter2","nested":{"DB_PASSWORD":"x","api-key":"k","list":[{"token":"t"}]}}`,
			wantArgs: `{"nested":{"DB_PASSWORD":"[REDACTED]","api-key":"[REDACTED]","list":[{"token":"[REDACTED]"}]},"password":"[REDACTED]","user":"sbx_a"}`,
		},
		{
			name:     "non-string secrets",
			args:     `{"secret":{"a":1},"Authorization":42,"session_cookie":null}`,
			wantArgs: `{"Authorization":"[REDACTED]","secret":"[REDACTED]","session_cookie":"[REDACTED]"}`,
		},
		{
			name:     "secrets inside strings",
			args:     `{"url":"mysql://sbx_a:s3cret@10.255.0.1:3306/app","header":"Bearer abc.def-ghi","cmd":"login --password=hunter2 --user bob"}`,
			wantArgs: `{"cmd":"login --password=[REDACTED] --user bob","header":"Bearer [REDACTED]","url":"mysql://sbx_a:[REDACTED]@10.255.0.1:3306/app"}`,
		},
		{
			name:       "detail is redacted too",
			args:       `{}`,
			detail:     `connect failed: token=abc123; DSN mysql://u:p@h/db`,
			wantArgs:   `{}`,
			wantDetail: `connect failed: token=[REDACTED]; DSN mysql://u:[REDACTED]@h/db`,
		},
		{
			name:     "harmless values stay",
			args:     `{"block":"mariadb","profile":"dev","port":3306,"big":12345678901234567890}`,
			wantArgs: `{"big":12345678901234567890,"block":"mariadb","port":3306,"profile":"dev"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			l := openLog(t)
			rec, err := l.Append(ctx, Event{Actor: "mcp:abcdefghijkl", Tool: "service_enable",
				Args: json.RawMessage(tc.args), Result: ResultOK, Detail: tc.detail})
			if err != nil {
				t.Fatal(err)
			}
			// Check what is stored, not only what Append returned.
			var stored, detail string
			if err := l.DB.QueryRowContext(ctx, `SELECT args_json, detail FROM audit_events WHERE seq = ?`,
				rec.Seq).Scan(&stored, &detail); err != nil {
				t.Fatal(err)
			}
			if stored != tc.wantArgs {
				t.Errorf("args\n got %s\nwant %s", stored, tc.wantArgs)
			}
			if detail != tc.wantDetail {
				t.Errorf("detail\n got %q\nwant %q", detail, tc.wantDetail)
			}
			for _, secret := range []string{"hunter2", "s3cret", "abc.def-ghi", "abc123"} {
				if strings.Contains(stored+detail, secret) {
					t.Errorf("secret %q stored", secret)
				}
			}
			if _, err := l.Verify(ctx); err != nil {
				t.Errorf("Verify: %v", err)
			}
		})
	}
}
