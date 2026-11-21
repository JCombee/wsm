package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SandboxService is one row of sandbox_services: a per-sandbox service a
// sandbox enabled, at one tag (ADR 0015).
type SandboxService struct {
	SandboxID string
	BlockID   string
	Tag       string
	CreatedAt time.Time
}

// SetSandboxService records that a sandbox runs blockID at tag. An
// existing row for the same block gets the new tag.
func (s *Store) SetSandboxService(ctx context.Context, sandboxID, blockID, tag string) error {
	if !ValidID(sandboxID) || blockID == "" || tag == "" {
		return fmt.Errorf("set sandbox service: %w", ErrInvalid)
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sandbox_services (sandbox_id, block_id, tag, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (sandbox_id, block_id) DO UPDATE SET tag = excluded.tag`,
		sandboxID, blockID, tag, formatTime(s.Now().UTC()))
	if err != nil {
		return fmt.Errorf("set service %s of sandbox %s: %w", blockID, sandboxID, mapConstraint(err))
	}
	return nil
}

// SandboxServices returns the per-sandbox services of a sandbox, ordered by
// block.
func (s *Store) SandboxServices(ctx context.Context, sandboxID string) ([]SandboxService, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT sandbox_id, block_id, tag, created_at FROM sandbox_services
		WHERE sandbox_id = ? ORDER BY block_id`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("services of sandbox %s: %w", sandboxID, err)
	}
	defer rows.Close()
	var out []SandboxService
	for rows.Next() {
		var sv SandboxService
		var created string
		if err := rows.Scan(&sv.SandboxID, &sv.BlockID, &sv.Tag, &created); err != nil {
			return nil, fmt.Errorf("services of sandbox %s: %w", sandboxID, err)
		}
		if sv.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("services of sandbox %s: %w", sandboxID, err)
		}
		out = append(out, sv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("services of sandbox %s: %w", sandboxID, err)
	}
	return out, nil
}

// SandboxServicesOutsideCeiling returns the slugs of the sandboxes of a
// project that run blockID at a tag outside tags, sorted. An empty tags
// means the whole block.
func (s *Store) SandboxServicesOutsideCeiling(ctx context.Context, projectID, blockID string, tags []string) ([]string, error) {
	q := `SELECT DISTINCT s.slug
		FROM sandbox_services v JOIN sandboxes s ON s.id = v.sandbox_id
		WHERE s.project_id = ? AND v.block_id = ?`
	args := []any{projectID, blockID}
	if len(tags) > 0 {
		q += ` AND v.tag NOT IN (?` + strings.Repeat(`, ?`, len(tags)-1) + `)`
		for _, t := range tags {
			args = append(args, t)
		}
	}
	q += ` ORDER BY s.slug`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sandbox services outside ceiling of project %s: %w", projectID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("sandbox services outside ceiling of project %s: %w", projectID, err)
		}
		out = append(out, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sandbox services outside ceiling of project %s: %w", projectID, err)
	}
	return out, nil
}

// DeleteSandboxService removes the record of a per-sandbox service.
func (s *Store) DeleteSandboxService(ctx context.Context, sandboxID, blockID string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sandbox_services WHERE sandbox_id = ? AND block_id = ?`, sandboxID, blockID)
	if err != nil {
		return fmt.Errorf("delete service %s of sandbox %s: %w", blockID, sandboxID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("service %s of sandbox %s: %w", blockID, sandboxID, ErrNotFound)
	}
	return nil
}
