package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/JCombee/wsm/internal/netpolicy"
)

// ProjectBlock is one row of project_blocks: the ceiling of a project for
// one block (ADR 0013).
type ProjectBlock struct {
	ProjectID string
	BlockID   string
	Profile   string
	// ConfigJSON is {"tags": [...]}. The database only checks that it is
	// JSON; the broker checks the content.
	ConfigJSON string
}

// mapConstraint turns a foreign key violation into ErrNotFound and a CHECK
// violation into ErrInvalid; other errors go through mapErr.
func mapConstraint(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return fmt.Errorf("%w: %v", ErrNotFound, err)
		case sqlite3.SQLITE_CONSTRAINT_CHECK:
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	return mapErr(err)
}

// SetProjectBlock inserts or replaces the ceiling of a project for a block.
func (s *Store) SetProjectBlock(ctx context.Context, b ProjectBlock) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO project_blocks (project_id, block_id, profile, config_json)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (project_id, block_id) DO UPDATE
		SET profile = excluded.profile, config_json = excluded.config_json`,
		b.ProjectID, b.BlockID, b.Profile, b.ConfigJSON)
	if err != nil {
		return fmt.Errorf("set block %s of project %s: %w", b.BlockID, b.ProjectID, mapConstraint(err))
	}
	return nil
}

// ProjectBlocks returns the ceilings of a project, ordered by block.
func (s *Store) ProjectBlocks(ctx context.Context, projectID string) ([]ProjectBlock, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT project_id, block_id, profile, config_json FROM project_blocks
		WHERE project_id = ? ORDER BY block_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list blocks of project %s: %w", projectID, err)
	}
	defer rows.Close()
	var out []ProjectBlock
	for rows.Next() {
		var b ProjectBlock
		if err := rows.Scan(&b.ProjectID, &b.BlockID, &b.Profile, &b.ConfigJSON); err != nil {
			return nil, fmt.Errorf("list blocks of project %s: %w", projectID, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list blocks of project %s: %w", projectID, err)
	}
	return out, nil
}

// DeleteProjectBlock removes the ceiling of a project for a block.
func (s *Store) DeleteProjectBlock(ctx context.Context, projectID, blockID string) error {
	res, err := s.DB.ExecContext(ctx, `
		DELETE FROM project_blocks WHERE project_id = ? AND block_id = ?`, projectID, blockID)
	if err != nil {
		return fmt.Errorf("delete block %s of project %s: %w", blockID, projectID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("block %s of project %s: %w", blockID, projectID, ErrNotFound)
	}
	return nil
}

// ServiceInstance is one row of service_instances: the shared instance of
// a block for one tag (ADR 0013).
type ServiceInstance struct {
	ID      string
	BlockID string
	Tag     string
	Port    int
	// SecretEnc is the sealed root password, bound to ID.
	SecretEnc []byte
	CreatedAt time.Time
}

// CreateServiceInstance inserts inst with the lowest free port of the
// service range, chosen in the same transaction. inst.ID must be set
// already, because the sealed secret is bound to it; inst.Port and
// inst.CreatedAt are filled in.
func (s *Store) CreateServiceInstance(ctx context.Context, inst *ServiceInstance) error {
	if !ValidID(inst.ID) {
		return fmt.Errorf("create service instance: %w: id %q", ErrInvalid, inst.ID)
	}
	if len(inst.SecretEnc) == 0 {
		return fmt.Errorf("create service instance: %w: no secret", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create service instance: %w", err)
	}
	defer tx.Rollback()
	var port sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT MIN(p) FROM (
			SELECT ? AS p
			UNION ALL SELECT port + 1 FROM service_instances
		) WHERE p <= ? AND p NOT IN (SELECT port FROM service_instances)`,
		netpolicy.ServicePortMin, netpolicy.ServicePortMax).Scan(&port)
	if err != nil {
		return fmt.Errorf("create service instance: allocate port: %w", err)
	}
	if !port.Valid {
		return fmt.Errorf("create service instance: no free port in %d-%d",
			netpolicy.ServicePortMin, netpolicy.ServicePortMax)
	}
	inst.Port = int(port.Int64)
	inst.CreatedAt = s.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO service_instances (id, block_id, tag, port, secret_enc, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		inst.ID, inst.BlockID, inst.Tag, inst.Port, inst.SecretEnc, formatTime(inst.CreatedAt))
	if err != nil {
		return fmt.Errorf("create service instance %s %s: %w", inst.BlockID, inst.Tag, mapConstraint(err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create service instance: %w", err)
	}
	return nil
}

const instanceColumns = `id, block_id, tag, port, secret_enc, created_at`

func scanInstance(row interface{ Scan(...any) error }) (ServiceInstance, error) {
	var inst ServiceInstance
	var created string
	if err := row.Scan(&inst.ID, &inst.BlockID, &inst.Tag, &inst.Port, &inst.SecretEnc, &created); err != nil {
		return inst, mapErr(err)
	}
	t, err := parseTime(created)
	if err != nil {
		return inst, err
	}
	inst.CreatedAt = t
	return inst, nil
}

// ServiceInstance returns the instance of a block for a tag.
func (s *Store) ServiceInstance(ctx context.Context, blockID, tag string) (ServiceInstance, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+instanceColumns+`
		FROM service_instances WHERE block_id = ? AND tag = ?`, blockID, tag)
	inst, err := scanInstance(row)
	if err != nil {
		return inst, fmt.Errorf("service instance %s %s: %w", blockID, tag, err)
	}
	return inst, nil
}

// ServiceInstances returns all instances, ordered by block and port.
func (s *Store) ServiceInstances(ctx context.Context) ([]ServiceInstance, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+instanceColumns+`
		FROM service_instances ORDER BY block_id, port`)
	if err != nil {
		return nil, fmt.Errorf("list service instances: %w", err)
	}
	defer rows.Close()
	var out []ServiceInstance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list service instances: %w", err)
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list service instances: %w", err)
	}
	return out, nil
}
