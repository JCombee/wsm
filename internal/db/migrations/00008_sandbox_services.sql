-- +goose Up
-- M3d: a per-sandbox service runs as a container in the sandbox's pod
-- (ADR 0015). The row records which tag the sandbox enabled; the container
-- itself is derived from it (sbx-<id>-<block>) and has no account.
--
-- Unlike service_grants, sandbox_id may cascade: the container goes with
-- the pod, which is removed before the sandbox row.

CREATE TABLE sandbox_services (
    sandbox_id TEXT NOT NULL REFERENCES sandboxes (id) ON DELETE CASCADE,
    block_id   TEXT NOT NULL,
    tag        TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (sandbox_id, block_id)
) STRICT;

-- +goose Down
DROP TABLE sandbox_services;
