// Package audit writes and verifies the append-only audit log (plan section
// 3.10). Every event is redacted before it is stored, and chained to the
// previous one: hash = sha256(prev_hash || canonical JSON of the event), with
// both hashes as lowercase hex. The first event chains to GenesisHash.
//
// SQLite triggers (migration 00002) reject UPDATE and DELETE on the table;
// the hash chain lets Verify detect changes made around them, for example by
// someone with write access to the database file.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/JCombee/wsm/internal/db"
)

// GenesisHash is the prev_hash of the first event.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// maxArgs caps the size of an event's arguments, before and after redaction.
const maxArgs = 64 << 10

// Results of an audited call.
const (
	ResultOK     = "ok"
	ResultDenied = "denied"
	ResultError  = "error"
)

// Event is one audited call, as the caller describes it.
type Event struct {
	Actor     string          // who made the call, e.g. "mcp:<sandbox id>"
	SandboxID string          // empty when the call is not about one sandbox
	Tool      string          // the tool or operation
	Args      json.RawMessage // the call's arguments as a JSON value; nil for none
	Result    string          // ResultOK, ResultDenied or ResultError
	Detail    string          // a short explanation, e.g. why it was denied
}

// Record is a stored event.
type Record struct {
	Seq       int64
	TS        string
	Actor     string
	SandboxID string
	Tool      string
	ArgsJSON  string
	Result    string
	Detail    string
	PrevHash  string
	Hash      string
}

// Log appends to and verifies the audit_events table.
type Log struct {
	DB  *sql.DB
	Now func() time.Time
}

// New returns a Log on sqlDB that uses the wall clock.
func New(sqlDB *sql.DB) *Log { return &Log{DB: sqlDB, Now: time.Now} }

// canonicalEvent is what the hash covers. Field order is fixed by the struct;
// Args holds canonical JSON (sorted keys, no insignificant whitespace).
type canonicalEvent struct {
	Seq       int64           `json:"seq"`
	TS        string          `json:"ts"`
	Actor     string          `json:"actor"`
	SandboxID string          `json:"sandbox_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Result    string          `json:"result"`
	Detail    string          `json:"detail"`
}

// canonicalJSON re-encodes a JSON value with sorted object keys and no
// insignificant whitespace. Numbers keep their literal form.
func canonicalJSON(raw []byte) ([]byte, error) {
	v, err := decodeStrict(raw)
	if err != nil {
		return nil, err
	}
	return encode(v)
}

func decodeStrict(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	// Not dec.More: it reports false before a closing bracket, so "{}}"
	// would pass. Only io.EOF proves that nothing follows the value.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode json: trailing data")
	}
	return v, nil
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode json: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// hashRecord computes the hash of r from its stored columns.
func hashRecord(r Record) (string, error) {
	args, err := canonicalJSON([]byte(r.ArgsJSON))
	if err != nil {
		return "", fmt.Errorf("event %d args: %w", r.Seq, err)
	}
	body, err := encode(canonicalEvent{
		Seq: r.Seq, TS: r.TS, Actor: r.Actor, SandboxID: r.SandboxID, Tool: r.Tool,
		Args: args, Result: r.Result, Detail: r.Detail,
	})
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(r.PrevHash))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Append redacts e and appends it to the log. It returns the stored record.
func (l *Log) Append(ctx context.Context, e Event) (Record, error) {
	switch e.Result {
	case ResultOK, ResultDenied, ResultError:
	default:
		return Record{}, fmt.Errorf("audit: invalid result %q", e.Result)
	}
	if e.Actor == "" || e.Tool == "" {
		return Record{}, errors.New("audit: actor and tool are required")
	}
	rawArgs := []byte(e.Args)
	if len(rawArgs) == 0 {
		rawArgs = []byte("{}")
	}
	if len(rawArgs) > maxArgs {
		return Record{}, fmt.Errorf("audit: args exceed %d bytes", maxArgs)
	}
	v, err := decodeStrict(rawArgs)
	if err != nil {
		return Record{}, fmt.Errorf("audit: args: %w", err)
	}
	args, err := encode(redactValue(v))
	if err != nil {
		return Record{}, fmt.Errorf("audit: args: %w", err)
	}

	r := Record{
		TS:        db.FormatTime(l.Now()),
		Actor:     redactString(e.Actor),
		SandboxID: e.SandboxID,
		Tool:      e.Tool,
		ArgsJSON:  string(args),
		Result:    e.Result,
		Detail:    redactString(e.Detail),
	}

	// The database opens transactions with BEGIN IMMEDIATE, so no other
	// writer can append between reading the head and inserting.
	tx, err := l.DB.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("audit: %w", err)
	}
	defer tx.Rollback()
	var lastSeq sql.NullInt64
	var lastHash sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT seq, hash FROM audit_events ORDER BY seq DESC LIMIT 1`).Scan(&lastSeq, &lastHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.Seq, r.PrevHash = 1, GenesisHash
	case err != nil:
		return Record{}, fmt.Errorf("audit: read chain head: %w", err)
	default:
		r.Seq, r.PrevHash = lastSeq.Int64+1, lastHash.String
	}
	if r.Hash, err = hashRecord(r); err != nil {
		return Record{}, fmt.Errorf("audit: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_events (seq, ts, actor, sandbox_id, tool, args_json,
			result, detail, prev_hash, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Seq, r.TS, r.Actor, nullable(r.SandboxID), r.Tool, r.ArgsJSON,
		r.Result, r.Detail, r.PrevHash, r.Hash)
	if err != nil {
		return Record{}, fmt.Errorf("audit: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("audit: commit: %w", err)
	}
	return r, nil
}

func nullable(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// VerifyError reports the first event where the chain is broken.
type VerifyError struct {
	Seq    int64
	Reason string
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("audit chain broken at event %d: %s", e.Seq, e.Reason)
}

// Verify walks the whole chain and returns the number of events checked. It
// returns a *VerifyError at the first event that does not match.
func (l *Log) Verify(ctx context.Context) (int64, error) {
	rows, err := l.DB.QueryContext(ctx, `
		SELECT seq, ts, actor, sandbox_id, tool, args_json, result, detail, prev_hash, hash
		FROM audit_events ORDER BY seq`)
	if err != nil {
		return 0, fmt.Errorf("audit verify: %w", err)
	}
	defer rows.Close()
	var n int64
	prev := GenesisHash
	for rows.Next() {
		var r Record
		var sandboxID sql.NullString
		if err := rows.Scan(&r.Seq, &r.TS, &r.Actor, &sandboxID, &r.Tool, &r.ArgsJSON,
			&r.Result, &r.Detail, &r.PrevHash, &r.Hash); err != nil {
			return n, fmt.Errorf("audit verify: %w", err)
		}
		r.SandboxID = sandboxID.String
		n++
		if r.Seq != n {
			return n, &VerifyError{Seq: r.Seq, Reason: fmt.Sprintf("expected sequence number %d", n)}
		}
		if r.PrevHash != prev {
			return n, &VerifyError{Seq: r.Seq, Reason: "prev_hash does not match the previous event"}
		}
		want, err := hashRecord(r)
		if err != nil {
			return n, &VerifyError{Seq: r.Seq, Reason: err.Error()}
		}
		if r.Hash != want {
			return n, &VerifyError{Seq: r.Seq, Reason: "hash does not match the event"}
		}
		prev = r.Hash
	}
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("audit verify: %w", err)
	}
	return n, nil
}

// List returns at most limit events, newest first, with seq below before
// (all when before is 0), optionally only those of one sandbox.
func (l *Log) List(ctx context.Context, before int64, limit int, sandboxID string) ([]Record, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT seq, ts, actor, sandbox_id, tool, args_json, result, detail, prev_hash, hash
		FROM audit_events WHERE 1 = 1`
	var args []any
	if before > 0 {
		q += ` AND seq < ?`
		args = append(args, before)
	}
	if sandboxID != "" {
		q += ` AND sandbox_id = ?`
		args = append(args, sandboxID)
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := l.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		var sb sql.NullString
		if err := rows.Scan(&r.Seq, &r.TS, &r.Actor, &sb, &r.Tool, &r.ArgsJSON, &r.Result, &r.Detail, &r.PrevHash, &r.Hash); err != nil {
			return nil, fmt.Errorf("audit: list: %w", err)
		}
		r.SandboxID = sb.String
		out = append(out, r)
	}
	return out, rows.Err()
}
