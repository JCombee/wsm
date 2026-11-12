-- +goose Up
-- Initial schema for M1 (docs/plan/bouwplan-fase-1.md, section 3.12). Only
-- the columns M1 uses; later milestones add theirs with their own migrations.

CREATE TABLE projects (
    id          TEXT PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    linux_user  TEXT NOT NULL UNIQUE,
    -- The range of internal/projectuid; TestUIDCheckMatchesRange keeps them equal.
    linux_uid   INTEGER NOT NULL UNIQUE CHECK (linux_uid BETWEEN 50000 AND 59999),
    image_ref   TEXT NOT NULL,
    cpu_limit   INTEGER NOT NULL CHECK (cpu_limit > 0),   -- millicores
    mem_limit   INTEGER NOT NULL CHECK (mem_limit > 0),   -- bytes
    pids_limit  INTEGER NOT NULL CHECK (pids_limit > 0),
    created_at  TEXT NOT NULL
) STRICT;

CREATE TABLE sandboxes (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    slug             TEXT NOT NULL,
    status           TEXT NOT NULL,
    pod_name         TEXT NOT NULL UNIQUE,
    created_at       TEXT NOT NULL,
    last_activity_at TEXT,
    UNIQUE (project_id, slug)
) STRICT;

-- sandbox_id has no foreign key: audit events outlive their sandbox.
CREATE TABLE audit_events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    sandbox_id TEXT,
    tool       TEXT NOT NULL,
    args_json  TEXT NOT NULL,
    result     TEXT NOT NULL,
    detail     TEXT NOT NULL,
    prev_hash  TEXT NOT NULL,
    hash       TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE jobs (
    id          TEXT PRIMARY KEY,
    type        TEXT NOT NULL,
    ref_id      TEXT NOT NULL,
    status      TEXT NOT NULL,
    error       TEXT,
    created_at  TEXT NOT NULL,
    finished_at TEXT
) STRICT;

CREATE INDEX jobs_ref_id ON jobs (ref_id);

-- +goose Down
DROP TABLE jobs;
DROP TABLE audit_events;
DROP TABLE sandboxes;
DROP TABLE projects;
