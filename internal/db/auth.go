package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// User is a dashboard user. Phase 1 has exactly one: the owner.
type User struct {
	ID        string
	Handle    []byte // WebAuthn user handle, 32 random bytes
	Name      string
	CreatedAt time.Time
}

// Passkey is a stored WebAuthn credential.
type Passkey struct {
	ID           string
	UserID       string
	CredentialID []byte
	// CredentialJSON is go-webauthn's Credential as JSON.
	CredentialJSON string
	Name           string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

// Session is a dashboard session. IDHash is the SHA-256 of the cookie
// value; the value itself is never stored.
type Session struct {
	IDHash     string
	UserID     string
	CSRFToken  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	UserAgent  string
}

// PreviewSession is a session on one preview host.
type PreviewSession struct {
	IDHash    string
	UserID    string
	Host      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created string
	if err := row.Scan(&u.ID, &u.Handle, &u.Name, &created); err != nil {
		return u, mapErr(err)
	}
	t, err := parseTime(created)
	u.CreatedAt = t
	return u, err
}

// EnsureUser returns the user called name, creating it with handle when it
// does not exist yet.
func (s *Store) EnsureUser(ctx context.Context, name string, handle []byte) (User, error) {
	if name == "" || len(handle) != 32 {
		return User{}, fmt.Errorf("ensure user: %w", ErrInvalid)
	}
	u, err := s.UserByName(ctx, name)
	if err == nil {
		return u, nil
	}
	id, err := NewID()
	if err != nil {
		return User{}, err
	}
	u = User{ID: id, Handle: handle, Name: name, CreatedAt: s.Now().UTC()}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO users (id, handle, name, created_at) VALUES (?, ?, ?, ?)`,
		u.ID, u.Handle, u.Name, formatTime(u.CreatedAt))
	if err != nil {
		return User{}, fmt.Errorf("create user %q: %w", name, mapErr(err))
	}
	return u, nil
}

// UserByName returns the user called name.
func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	u, err := scanUser(s.DB.QueryRowContext(ctx, `SELECT id, handle, name, created_at FROM users WHERE name = ?`, name))
	if err != nil {
		return u, fmt.Errorf("user %q: %w", name, err)
	}
	return u, nil
}

// UserByID returns the user with id.
func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	u, err := scanUser(s.DB.QueryRowContext(ctx, `SELECT id, handle, name, created_at FROM users WHERE id = ?`, id))
	if err != nil {
		return u, fmt.Errorf("user %s: %w", id, err)
	}
	return u, nil
}

// UserByHandle returns the user with a WebAuthn user handle.
func (s *Store) UserByHandle(ctx context.Context, handle []byte) (User, error) {
	u, err := scanUser(s.DB.QueryRowContext(ctx, `SELECT id, handle, name, created_at FROM users WHERE handle = ?`, handle))
	if err != nil {
		return u, fmt.Errorf("user by handle: %w", err)
	}
	return u, nil
}

// CountPasskeys returns the number of stored passkeys.
func (s *Store) CountPasskeys(ctx context.Context) (int, error) {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM passkeys`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count passkeys: %w", err)
	}
	return n, nil
}

// AddPasskey stores a new passkey; pk.ID and pk.CreatedAt are filled in.
func (s *Store) AddPasskey(ctx context.Context, pk *Passkey) error {
	if len(pk.CredentialID) == 0 || pk.CredentialJSON == "" || !ValidID(pk.UserID) {
		return fmt.Errorf("add passkey: %w", ErrInvalid)
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	pk.ID = id
	pk.CreatedAt = s.Now().UTC()
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO passkeys (id, user_id, credential_id, credential_json, name, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		pk.ID, pk.UserID, pk.CredentialID, pk.CredentialJSON, pk.Name, formatTime(pk.CreatedAt))
	if err != nil {
		return fmt.Errorf("add passkey: %w", mapErr(err))
	}
	return nil
}

// Passkeys returns the passkeys of a user, oldest first.
func (s *Store) Passkeys(ctx context.Context, userID string) ([]Passkey, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, user_id, credential_id, credential_json, name, created_at, last_used_at
		FROM passkeys WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var pk Passkey
		var created string
		var used sql.NullString
		if err := rows.Scan(&pk.ID, &pk.UserID, &pk.CredentialID, &pk.CredentialJSON, &pk.Name, &created, &used); err != nil {
			return nil, fmt.Errorf("list passkeys: %w", err)
		}
		if pk.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if used.Valid {
			t, err := parseTime(used.String)
			if err != nil {
				return nil, err
			}
			pk.LastUsedAt = &t
		}
		out = append(out, pk)
	}
	return out, rows.Err()
}

// UpdatePasskeyUse stores a passkey's credential after a login (its sign
// count changes) and its last use.
func (s *Store) UpdatePasskeyUse(ctx context.Context, credentialID []byte, credentialJSON string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE passkeys SET credential_json = ?, last_used_at = ? WHERE credential_id = ?`,
		credentialJSON, formatTime(s.Now().UTC()), credentialID)
	if err != nil {
		return fmt.Errorf("update passkey: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update passkey: %w", ErrNotFound)
	}
	return nil
}

// DeletePasskey removes a passkey of a user.
func (s *Store) DeletePasskey(ctx context.Context, userID, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM passkeys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete passkey: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete passkey %s: %w", id, ErrNotFound)
	}
	return nil
}

// CreateSession stores a session; CreatedAt and LastSeenAt are filled in.
func (s *Store) CreateSession(ctx context.Context, se *Session) error {
	now := s.Now().UTC()
	se.CreatedAt, se.LastSeenAt = now, now
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sessions (id_hash, user_id, csrf_token, created_at, expires_at, last_seen_at, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		se.IDHash, se.UserID, se.CSRFToken, formatTime(now), formatTime(se.ExpiresAt), formatTime(now), se.UserAgent)
	if err != nil {
		return fmt.Errorf("create session: %w", mapErr(err))
	}
	return nil
}

// SessionByHash returns an unexpired session.
func (s *Store) SessionByHash(ctx context.Context, idHash string) (Session, error) {
	var se Session
	var created, expires, seen string
	err := s.DB.QueryRowContext(ctx, `
		SELECT id_hash, user_id, csrf_token, created_at, expires_at, last_seen_at, user_agent
		FROM sessions WHERE id_hash = ? AND expires_at > ?`, idHash, formatTime(s.Now().UTC())).
		Scan(&se.IDHash, &se.UserID, &se.CSRFToken, &created, &expires, &seen, &se.UserAgent)
	if err != nil {
		return se, fmt.Errorf("session: %w", mapErr(err))
	}
	if se.CreatedAt, err = parseTime(created); err != nil {
		return se, err
	}
	if se.ExpiresAt, err = parseTime(expires); err != nil {
		return se, err
	}
	se.LastSeenAt, err = parseTime(seen)
	return se, err
}

// TouchSession records that a session was used.
func (s *Store) TouchSession(ctx context.Context, idHash string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?`,
		formatTime(s.Now().UTC()), idHash)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredAuth removes expired sessions, tokens and preview sessions.
func (s *Store) DeleteExpiredAuth(ctx context.Context) error {
	now := formatTime(s.Now().UTC())
	for _, table := range []string{"sessions", "enroll_tokens", "preview_tokens", "preview_sessions"} {
		if _, err := s.DB.ExecContext(ctx, `DELETE FROM `+table+` WHERE expires_at <= ?`, now); err != nil {
			return fmt.Errorf("delete expired %s: %w", table, err)
		}
	}
	return nil
}

// CreateEnrollToken stores the hash of a one-time registration token.
func (s *Store) CreateEnrollToken(ctx context.Context, tokenHash string, expires time.Time) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO enroll_tokens (token_hash, created_at, expires_at) VALUES (?, ?, ?)`,
		tokenHash, formatTime(s.Now().UTC()), formatTime(expires))
	if err != nil {
		return fmt.Errorf("create enroll token: %w", mapErr(err))
	}
	return nil
}

// CheckEnrollToken reports whether an unexpired enroll token exists,
// without using it up.
func (s *Store) CheckEnrollToken(ctx context.Context, tokenHash string) error {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM enroll_tokens WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, formatTime(s.Now().UTC())).Scan(&n)
	if err != nil {
		return fmt.Errorf("check enroll token: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("enroll token: %w", ErrNotFound)
	}
	return nil
}

// ConsumeEnrollToken deletes an enroll token and fails when it did not
// exist or had expired.
func (s *Store) ConsumeEnrollToken(ctx context.Context, tokenHash string) error {
	var expires string
	err := s.DB.QueryRowContext(ctx, `DELETE FROM enroll_tokens WHERE token_hash = ? RETURNING expires_at`, tokenHash).Scan(&expires)
	if err != nil {
		return fmt.Errorf("enroll token: %w", mapErr(err))
	}
	t, err := parseTime(expires)
	if err != nil {
		return err
	}
	if !t.After(s.Now()) {
		return fmt.Errorf("enroll token: %w", ErrNotFound)
	}
	return nil
}

// CreatePreviewToken stores the hash of a one-time preview token.
func (s *Store) CreatePreviewToken(ctx context.Context, tokenHash, userID, host string, expires time.Time) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO preview_tokens (token_hash, user_id, host, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, userID, host, formatTime(expires))
	if err != nil {
		return fmt.Errorf("create preview token: %w", mapErr(err))
	}
	return nil
}

// ConsumePreviewToken deletes a preview token and returns its user. It
// fails, after deleting it, when the token expired or belongs to another
// host.
func (s *Store) ConsumePreviewToken(ctx context.Context, tokenHash, host string) (string, error) {
	var userID, tokenHost, expires string
	err := s.DB.QueryRowContext(ctx, `DELETE FROM preview_tokens WHERE token_hash = ? RETURNING user_id, host, expires_at`,
		tokenHash).Scan(&userID, &tokenHost, &expires)
	if err != nil {
		return "", fmt.Errorf("preview token: %w", mapErr(err))
	}
	t, err := parseTime(expires)
	if err != nil {
		return "", err
	}
	if tokenHost != host || !t.After(s.Now()) {
		return "", fmt.Errorf("preview token: %w", ErrNotFound)
	}
	return userID, nil
}

// CreatePreviewSession stores a preview session; CreatedAt is filled in.
func (s *Store) CreatePreviewSession(ctx context.Context, ps *PreviewSession) error {
	ps.CreatedAt = s.Now().UTC()
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO preview_sessions (id_hash, user_id, host, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		ps.IDHash, ps.UserID, ps.Host, formatTime(ps.CreatedAt), formatTime(ps.ExpiresAt))
	if err != nil {
		return fmt.Errorf("create preview session: %w", mapErr(err))
	}
	return nil
}

// PreviewSessionValid reports whether an unexpired preview session with
// idHash exists for host.
func (s *Store) PreviewSessionValid(ctx context.Context, idHash, host string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM preview_sessions WHERE id_hash = ? AND host = ? AND expires_at > ?`,
		idHash, host, formatTime(s.Now().UTC())).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("preview session: %w", err)
	}
	return n > 0, nil
}
