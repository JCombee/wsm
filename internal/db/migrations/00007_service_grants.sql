-- +goose Up
-- M3c: a grant gives one sandbox an account on one shared service instance
-- (ADR 0014). The account and database names derive from the sandbox ID
-- (sbx_<id>, sbx_<id>_app) and are not stored. secret_enc is the account
-- password, sealed with AES-256-GCM and bound to the grant ID.
--
-- sandbox_id deliberately has no ON DELETE CASCADE: a sandbox row can only
-- go once its grants are revoked (CLAUDE.md invariant 7).

CREATE TABLE service_grants (
    id          TEXT PRIMARY KEY,
    sandbox_id  TEXT NOT NULL REFERENCES sandboxes (id),
    instance_id TEXT NOT NULL REFERENCES service_instances (id),
    secret_enc  BLOB NOT NULL,
    created_at  TEXT NOT NULL,
    UNIQUE (sandbox_id, instance_id)
) STRICT;

-- +goose Down
DROP TABLE service_grants;
