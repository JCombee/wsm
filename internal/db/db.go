// Package db opens the manager's SQLite database, applies the embedded
// migrations and stores projects, sandboxes and jobs.
package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrInvalid is returned for values the database must not store.
var ErrInvalid = errors.New("invalid")

// ErrConflict is returned when a row violates a uniqueness rule.
var ErrConflict = errors.New("already exists")

// Open opens (and creates) the database at path and applies all migrations.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := Migrate(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return sqlDB, nil
}

// OpenReadOnly opens an existing database without changing it: no
// migrations, no schema changes. It is meant for offline checks such as
// "wsm audit verify".
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	return sqlDB, nil
}

// Migrate applies every pending embedded migration.
func Migrate(ctx context.Context, sqlDB *sql.DB) error {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, fsys,
		goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// idAlphabet holds 32 characters, so one random byte maps to one character
// without bias. IDs end up in Linux, Podman and (later) MariaDB names, so they
// are lowercase letters and digits only.
const idAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// IDLength is the length of every generated ID.
const IDLength = 12

var idPattern = regexp.MustCompile(`^[a-z2-7]{12}$`)

// NewID returns a random ID of IDLength characters.
func NewID() (string, error) {
	b := make([]byte, IDLength)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	for i := range b {
		b[i] = idAlphabet[b[i]%32]
	}
	return string(b), nil
}

// ValidID reports whether s has the form NewID produces.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// timeFormat has a fixed width, so stored timestamps sort as strings.
const timeFormat = "2006-01-02T15:04:05.000000000Z"

// FormatTime formats t (in UTC) the way the database stores timestamps.
func FormatTime(t time.Time) string { return formatTime(t) }

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}
