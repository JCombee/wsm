-- +goose Up
-- M3b: the ceiling of a project per block, and the shared service
-- instances (ADR 0013).
--
-- project_blocks.config_json is {"tags": [...]}: the tags (versions) the
-- project may use, the first one being the default. Only the API writes
-- it (CLAUDE.md invariant 5).
--
-- service_instances has one row per (block, tag). The port is in the
-- service range of internal/netpolicy and never changes. secret_enc is the
-- instance's root password, sealed with AES-256-GCM and bound to the
-- instance ID.

CREATE TABLE project_blocks (
    project_id  TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    block_id    TEXT NOT NULL,
    profile     TEXT NOT NULL,
    config_json TEXT NOT NULL CHECK (json_valid(config_json)),
    PRIMARY KEY (project_id, block_id)
) STRICT;

CREATE TABLE service_instances (
    id          TEXT PRIMARY KEY,
    block_id    TEXT NOT NULL,
    tag         TEXT NOT NULL,
    port        INTEGER NOT NULL UNIQUE CHECK (port BETWEEN 20000 AND 20999),
    secret_enc  BLOB NOT NULL,
    created_at  TEXT NOT NULL,
    UNIQUE (block_id, tag)
) STRICT;

-- +goose Down
DROP TABLE service_instances;
DROP TABLE project_blocks;
