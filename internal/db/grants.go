package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Grant is one row of service_grants, with the block, tag and port of its
// instance (ADR 0014).
type Grant struct {
	ID         string
	SandboxID  string
	InstanceID string
	// SecretEnc is the sealed account password, bound to ID.
	SecretEnc []byte
	CreatedAt time.Time

	// From the instance; filled in by the reads, ignored by CreateGrant.
	BlockID string
	Tag     string
	Port    int
}

// GrantTarget is one (project UID, instance port) pair that needs an
// nftables element.
type GrantTarget struct {
	UID  int
	Port int
}

// CreateGrant inserts g. g.ID must be set already, because the sealed
// secret is bound to it; g.CreatedAt is filled in.
func (s *Store) CreateGrant(ctx context.Context, g *Grant) error {
	if !ValidID(g.ID) || !ValidID(g.SandboxID) || !ValidID(g.InstanceID) {
		return fmt.Errorf("create grant: %w: id", ErrInvalid)
	}
	if len(g.SecretEnc) == 0 {
		return fmt.Errorf("create grant: %w: no secret", ErrInvalid)
	}
	g.CreatedAt = s.Now().UTC()
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO service_grants (id, sandbox_id, instance_id, secret_enc, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		g.ID, g.SandboxID, g.InstanceID, g.SecretEnc, formatTime(g.CreatedAt))
	if err != nil {
		return fmt.Errorf("create grant for sandbox %s: %w", g.SandboxID, mapConstraint(err))
	}
	return nil
}

const grantSelect = `SELECT g.id, g.sandbox_id, g.instance_id, g.secret_enc, g.created_at,
	i.block_id, i.tag, i.port
	FROM service_grants g JOIN service_instances i ON i.id = g.instance_id`

func scanGrant(row interface{ Scan(...any) error }) (Grant, error) {
	var g Grant
	var created string
	if err := row.Scan(&g.ID, &g.SandboxID, &g.InstanceID, &g.SecretEnc, &created,
		&g.BlockID, &g.Tag, &g.Port); err != nil {
		return g, mapErr(err)
	}
	t, err := parseTime(created)
	if err != nil {
		return g, err
	}
	g.CreatedAt = t
	return g, nil
}

// Grant returns the grant of a sandbox on an instance.
func (s *Store) Grant(ctx context.Context, sandboxID, instanceID string) (Grant, error) {
	g, err := scanGrant(s.DB.QueryRowContext(ctx, grantSelect+`
		WHERE g.sandbox_id = ? AND g.instance_id = ?`, sandboxID, instanceID))
	if err != nil {
		return g, fmt.Errorf("grant of sandbox %s: %w", sandboxID, err)
	}
	return g, nil
}

// GrantsBySandbox returns the grants of a sandbox, ordered by block and tag.
func (s *Store) GrantsBySandbox(ctx context.Context, sandboxID string) ([]Grant, error) {
	rows, err := s.DB.QueryContext(ctx, grantSelect+`
		WHERE g.sandbox_id = ? ORDER BY i.block_id, i.port`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("grants of sandbox %s: %w", sandboxID, err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("grants of sandbox %s: %w", sandboxID, err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("grants of sandbox %s: %w", sandboxID, err)
	}
	return out, nil
}

// CountProjectGrants returns how many grants the sandboxes of a project
// have on an instance.
func (s *Store) CountProjectGrants(ctx context.Context, projectID, instanceID string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM service_grants g JOIN sandboxes s ON s.id = g.sandbox_id
		WHERE s.project_id = ? AND g.instance_id = ?`, projectID, instanceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count grants of project %s: %w", projectID, err)
	}
	return n, nil
}

// GrantTargets returns every distinct (project UID, instance port) pair
// that has at least one grant, ordered by UID and port.
func (s *Store) GrantTargets(ctx context.Context) ([]GrantTarget, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT p.linux_uid, i.port
		FROM service_grants g
		JOIN sandboxes s ON s.id = g.sandbox_id
		JOIN projects p ON p.id = s.project_id
		JOIN service_instances i ON i.id = g.instance_id
		ORDER BY p.linux_uid, i.port`)
	if err != nil {
		return nil, fmt.Errorf("grant targets: %w", err)
	}
	defer rows.Close()
	var out []GrantTarget
	for rows.Next() {
		var t GrantTarget
		if err := rows.Scan(&t.UID, &t.Port); err != nil {
			return nil, fmt.Errorf("grant targets: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("grant targets: %w", err)
	}
	return out, nil
}

// GrantsOutsideCeiling returns the slugs of the sandboxes of a project
// that have a grant on block outside tags, sorted and without duplicates.
// An empty tags means the whole block.
func (s *Store) GrantsOutsideCeiling(ctx context.Context, projectID, blockID string, tags []string) ([]string, error) {
	q := `SELECT DISTINCT s.slug
		FROM service_grants g
		JOIN sandboxes s ON s.id = g.sandbox_id
		JOIN service_instances i ON i.id = g.instance_id
		WHERE s.project_id = ? AND i.block_id = ?`
	args := []any{projectID, blockID}
	if len(tags) > 0 {
		q += ` AND i.tag NOT IN (?` + strings.Repeat(`, ?`, len(tags)-1) + `)`
		for _, t := range tags {
			args = append(args, t)
		}
	}
	q += ` ORDER BY s.slug`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("grants outside ceiling of project %s: %w", projectID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("grants outside ceiling of project %s: %w", projectID, err)
		}
		out = append(out, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("grants outside ceiling of project %s: %w", projectID, err)
	}
	return out, nil
}

// DeleteGrant removes a grant row.
func (s *Store) DeleteGrant(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM service_grants WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete grant %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete grant %s: %w", id, ErrNotFound)
	}
	return nil
}

// ClearMCPToken removes the MCP token hash of a sandbox, so its token no
// longer authenticates. Clearing an already empty hash is not an error.
func (s *Store) ClearMCPToken(ctx context.Context, sandboxID string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE sandboxes SET mcp_token_hash = NULL WHERE id = ?`, sandboxID)
	if err != nil {
		return fmt.Errorf("clear mcp token of sandbox %s: %w", sandboxID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("clear mcp token of sandbox %s: %w", sandboxID, ErrNotFound)
	}
	return nil
}
