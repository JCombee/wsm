package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/JCombee/wsm/internal/projectuid"
)

// Project is one row of the projects table.
type Project struct {
	ID        string
	Slug      string
	Name      string
	LinuxUser string
	LinuxUID  int
	ImageRef  string
	CPUMillis int64 // CPU limit per sandbox in millicores
	MemBytes  int64 // memory limit per sandbox in bytes
	PidsLimit int64 // process limit per sandbox
	CreatedAt time.Time
}

// Sandbox statuses.
const (
	StatusCreated = "created"
	StatusRunning = "running"
	StatusStopped = "stopped"
	StatusFailed  = "failed"
)

// Sandbox is one row of the sandboxes table.
type Sandbox struct {
	ID             string
	ProjectID      string
	Slug           string
	Status         string
	PodName        string
	CreatedAt      time.Time
	LastActivityAt *time.Time
}

// Store reads and writes projects, sandboxes and jobs.
type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

// NewStore returns a Store on sqlDB that uses the wall clock.
func NewStore(sqlDB *sql.DB) *Store { return &Store{DB: sqlDB, Now: time.Now} }

func mapErr(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// CreateProject inserts p with the lowest free UID from the project range.
// userName derives the Linux user name from that UID; p.ID, p.LinuxUID,
// p.LinuxUser and p.CreatedAt are filled in.
func (s *Store) CreateProject(ctx context.Context, p *Project, userName func(uid int) string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	defer tx.Rollback()

	var uid sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT MIN(u) FROM (
			SELECT ? AS u
			UNION ALL SELECT linux_uid + 1 FROM projects
		) WHERE u <= ? AND u NOT IN (SELECT linux_uid FROM projects)`,
		projectuid.Min, projectuid.Max).Scan(&uid)
	if err != nil {
		return fmt.Errorf("create project: allocate uid: %w", err)
	}
	if !uid.Valid {
		return fmt.Errorf("create project: no free uid in %d-%d", projectuid.Min, projectuid.Max)
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	p.ID = id
	p.LinuxUID = int(uid.Int64)
	p.LinuxUser = userName(p.LinuxUID)
	p.CreatedAt = s.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO projects (id, slug, name, linux_user, linux_uid, image_ref,
			cpu_limit, mem_limit, pids_limit, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Slug, p.Name, p.LinuxUser, p.LinuxUID, p.ImageRef,
		p.CPUMillis, p.MemBytes, p.PidsLimit, formatTime(p.CreatedAt))
	if err != nil {
		return fmt.Errorf("create project %q: %w", p.Slug, mapErr(err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	return nil
}

const projectColumns = `id, slug, name, linux_user, linux_uid, image_ref,
	cpu_limit, mem_limit, pids_limit, created_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var created string
	if err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.LinuxUser, &p.LinuxUID, &p.ImageRef,
		&p.CPUMillis, &p.MemBytes, &p.PidsLimit, &created); err != nil {
		return p, mapErr(err)
	}
	t, err := parseTime(created)
	if err != nil {
		return p, err
	}
	p.CreatedAt = t
	return p, nil
}

// ProjectBySlug returns the project with the given slug.
func (s *Store) ProjectBySlug(ctx context.Context, slug string) (Project, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE slug = ?`, slug)
	p, err := scanProject(row)
	if err != nil {
		return p, fmt.Errorf("project %q: %w", slug, err)
	}
	return p, nil
}

// ProjectByID returns the project with the given ID.
func (s *Store) ProjectByID(ctx context.Context, id string) (Project, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	p, err := scanProject(row)
	if err != nil {
		return p, fmt.Errorf("project %s: %w", id, err)
	}
	return p, nil
}

// Projects returns all projects ordered by slug.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return out, nil
}

// DeleteProject removes a project row. It fails while sandboxes exist.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project %s: %w", id, mapErr(err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete project %s: %w", id, ErrNotFound)
	}
	return nil
}

// CreateSandbox inserts sb; sb.ID, sb.PodName and sb.CreatedAt are filled in.
// podName derives the pod name from the new ID.
func (s *Store) CreateSandbox(ctx context.Context, sb *Sandbox, podName func(id string) string) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	sb.ID = id
	sb.PodName = podName(id)
	sb.CreatedAt = s.Now().UTC()
	if sb.Status == "" {
		sb.Status = StatusCreated
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO sandboxes (id, project_id, slug, status, pod_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.ProjectID, sb.Slug, sb.Status, sb.PodName, formatTime(sb.CreatedAt))
	if err != nil {
		return fmt.Errorf("create sandbox %q: %w", sb.Slug, mapErr(err))
	}
	return nil
}

const sandboxColumns = `id, project_id, slug, status, pod_name, created_at, last_activity_at`

func scanSandbox(row interface{ Scan(...any) error }) (Sandbox, error) {
	var sb Sandbox
	var created string
	var activity sql.NullString
	if err := row.Scan(&sb.ID, &sb.ProjectID, &sb.Slug, &sb.Status, &sb.PodName,
		&created, &activity); err != nil {
		return sb, mapErr(err)
	}
	t, err := parseTime(created)
	if err != nil {
		return sb, err
	}
	sb.CreatedAt = t
	if activity.Valid {
		t, err := parseTime(activity.String)
		if err != nil {
			return sb, err
		}
		sb.LastActivityAt = &t
	}
	return sb, nil
}

// SandboxBySlug returns the sandbox with slug in a project.
func (s *Store) SandboxBySlug(ctx context.Context, projectID, slug string) (Sandbox, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+sandboxColumns+`
		FROM sandboxes WHERE project_id = ? AND slug = ?`, projectID, slug)
	sb, err := scanSandbox(row)
	if err != nil {
		return sb, fmt.Errorf("sandbox %q: %w", slug, err)
	}
	return sb, nil
}

// Sandboxes returns the sandboxes of a project, or of all projects when
// projectID is empty, ordered by creation time.
func (s *Store) Sandboxes(ctx context.Context, projectID string) ([]Sandbox, error) {
	q := `SELECT ` + sandboxColumns + ` FROM sandboxes`
	var args []any
	if projectID != "" {
		q += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list sandboxes: %w", err)
		}
		out = append(out, sb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return out, nil
}

// SetSandboxStatus updates the status of a sandbox.
func (s *Store) SetSandboxStatus(ctx context.Context, id, status string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE sandboxes SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("set sandbox %s status: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set sandbox %s status: %w", id, ErrNotFound)
	}
	return nil
}

// TouchSandbox records activity (terminal, later MCP) on a sandbox.
func (s *Store) TouchSandbox(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sandboxes SET last_activity_at = ? WHERE id = ?`,
		formatTime(s.Now()), id)
	if err != nil {
		return fmt.Errorf("touch sandbox %s: %w", id, err)
	}
	return nil
}

// DeleteSandbox removes a sandbox row.
func (s *Store) DeleteSandbox(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sandboxes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete sandbox %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete sandbox %s: %w", id, ErrNotFound)
	}
	return nil
}

// Job statuses.
const (
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// Job is one row of the jobs table.
type Job struct {
	ID         string
	Type       string
	RefID      string
	Status     string
	Error      string
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// StartJob records a running job of typ for refID and returns its ID.
func (s *Store) StartJob(ctx context.Context, typ, refID string) (string, error) {
	id, err := NewID()
	if err != nil {
		return "", err
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO jobs (id, type, ref_id, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, typ, refID, JobRunning, formatTime(s.Now()))
	if err != nil {
		return "", fmt.Errorf("start job %s: %w", typ, err)
	}
	return id, nil
}

// FinishJob marks a job as succeeded, or failed with jobErr's message.
func (s *Store) FinishJob(ctx context.Context, id string, jobErr error) error {
	status, msg := JobSucceeded, sql.NullString{}
	if jobErr != nil {
		status, msg = JobFailed, sql.NullString{String: jobErr.Error(), Valid: true}
	}
	_, err := s.DB.ExecContext(ctx, `
		UPDATE jobs SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, msg, formatTime(s.Now()), id)
	if err != nil {
		return fmt.Errorf("finish job %s: %w", id, err)
	}
	return nil
}

// JobsFor returns the jobs for refID, oldest first.
func (s *Store) JobsFor(ctx context.Context, refID string) ([]Job, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, type, ref_id, status, error, created_at, finished_at
		FROM jobs WHERE ref_id = ? ORDER BY created_at, id`, refID)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var errMsg, finished sql.NullString
		var created string
		if err := rows.Scan(&j.ID, &j.Type, &j.RefID, &j.Status, &errMsg, &created, &finished); err != nil {
			return nil, fmt.Errorf("list jobs: %w", err)
		}
		j.Error = errMsg.String
		if j.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if finished.Valid {
			t, err := parseTime(finished.String)
			if err != nil {
				return nil, err
			}
			j.FinishedAt = &t
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
