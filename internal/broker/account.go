package broker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/JCombee/wsm/internal/db"
	"github.com/JCombee/wsm/internal/helper"
)

// maxUserConnections caps the connections of one sandbox account (plan
// section 3.6, profile dev).
const maxUserConnections = 50

// SQLConn is one root session on a service instance. The broker sends
// every statement on its own: no multi-statements and no client-side
// interpolation (ADR 0014).
type SQLConn interface {
	// Exec runs one statement.
	Exec(ctx context.Context, query string, args ...any) error
	// Strings runs a query whose rows have one column and returns them.
	Strings(ctx context.Context, query string, args ...any) ([]string, error)
	Close() error
}

// accountName returns the account of a sandbox: sbx_<id>. The ID is
// checked first, because it ends up in SQL.
func accountName(sandboxID string) (string, error) {
	if !db.ValidID(sandboxID) {
		return "", fmt.Errorf("%w: sandbox id %q", ErrInvalid, sandboxID)
	}
	return "sbx_" + sandboxID, nil
}

// appDatabase returns the database a grant creates: sbx_<id>_app.
func appDatabase(sandboxID string) string { return "sbx_" + sandboxID + "_app" }

// likePattern matches the databases of a sandbox, sbx_<id>_*, with the
// underscores escaped so they only match themselves.
func likePattern(sandboxID string) string { return `sbx\_` + sandboxID + `\_%` }

// quoteIdent quotes a database name with backticks, doubling every
// backtick inside. Names from information_schema come from the agent.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// createAccountSQL returns the idempotent statements that create or repair
// the account of a sandbox with password: proven in the M3c preflight to
// keep data when run twice.
func createAccountSQL(sandboxID, password string) ([]string, error) {
	acct, err := accountName(sandboxID)
	if err != nil {
		return nil, err
	}
	if !helper.ValidPassword(password) {
		return nil, fmt.Errorf("%w: account password has the wrong form", ErrInvalid)
	}
	user := "'" + acct + "'@'%'"
	return []string{
		fmt.Sprintf("CREATE USER IF NOT EXISTS %s IDENTIFIED BY '%s' WITH MAX_USER_CONNECTIONS %d", user, password, maxUserConnections),
		fmt.Sprintf("ALTER USER %s IDENTIFIED BY '%s' WITH MAX_USER_CONNECTIONS %d", user, password, maxUserConnections),
		fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO %s", likePattern(sandboxID), user),
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", quoteIdent(appDatabase(sandboxID))),
	}, nil
}

// createAccount runs createAccountSQL on conn. An error never carries the
// password: a server message may quote the statement.
func createAccount(ctx context.Context, conn SQLConn, sandboxID, password string) error {
	stmts, err := createAccountSQL(sandboxID, password)
	if err != nil {
		return err
	}
	for i, q := range stmts {
		if err := conn.Exec(ctx, q); err != nil {
			msg := strings.ReplaceAll(err.Error(), password, "[redacted]")
			return fmt.Errorf("create account sbx_%s: statement %d: %s", sandboxID, i+1, msg)
		}
	}
	return nil
}

// errUnknownThread is MariaDB's error for KILL of a session that is gone.
const errUnknownThread = 1094

// revokeAccount removes the account of a sandbox and its databases, in the
// proven order: DROP USER first, so no new login gets in, then KILL the
// sessions that are still open, then drop every sbx_<id>_* database.
func revokeAccount(ctx context.Context, conn SQLConn, sandboxID string) error {
	acct, err := accountName(sandboxID)
	if err != nil {
		return err
	}
	if err := conn.Exec(ctx, "DROP USER IF EXISTS '"+acct+"'@'%'"); err != nil {
		return fmt.Errorf("revoke %s: drop user: %w", acct, err)
	}
	ids, err := conn.Strings(ctx, "SELECT ID FROM information_schema.PROCESSLIST WHERE USER = ?", acct)
	if err != nil {
		return fmt.Errorf("revoke %s: list sessions: %w", acct, err)
	}
	for _, id := range ids {
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			return fmt.Errorf("revoke %s: session id %q: %w", acct, id, err)
		}
		if err := conn.Exec(ctx, "KILL CONNECTION "+strconv.FormatUint(n, 10)); err != nil {
			var me *mysql.MySQLError
			if errors.As(err, &me) && me.Number == errUnknownThread {
				continue
			}
			return fmt.Errorf("revoke %s: kill session %d: %w", acct, n, err)
		}
	}
	names, err := conn.Strings(ctx,
		"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ?", likePattern(sandboxID))
	if err != nil {
		return fmt.Errorf("revoke %s: list databases: %w", acct, err)
	}
	prefix := acct + "_"
	for _, name := range names {
		// LIKE may compare case-insensitively; drop only what is ours.
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)); err != nil {
			return fmt.Errorf("revoke %s: drop database: %w", acct, err)
		}
	}
	return nil
}

// mysqlConn is SQLConn over go-sql-driver/mysql.
type mysqlConn struct{ db *sql.DB }

func (c *mysqlConn) Exec(ctx context.Context, query string, args ...any) error {
	_, err := c.db.ExecContext(ctx, query, args...)
	return err
}

func (c *mysqlConn) Strings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (c *mysqlConn) Close() error { return c.db.Close() }

// connectRoot opens one root session on the instance on port: plain TCP
// on wsm0, no TLS, no multi-statements and no interpolation, one
// connection at most.
func (b *Broker) connectRoot(ctx context.Context, port int, password string) (SQLConn, error) {
	if b.SQL != nil {
		return b.SQL(ctx, port, password)
	}
	cfg := mysql.NewConfig()
	cfg.User = "root"
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = b.address(port)
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	cfg.MultiStatements = false
	cfg.InterpolateParams = false
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to port %d: %w", port, err)
	}
	sqlDB := sql.OpenDB(connector)
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connect to port %d: %w", port, err)
	}
	return &mysqlConn{db: sqlDB}, nil
}
