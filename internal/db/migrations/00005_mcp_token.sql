-- +goose Up
-- M3a: every sandbox gets its own MCP bearer token (ADR 0012). Only the
-- SHA-256 of the token is stored, as lowercase hex. Sandboxes created
-- before this migration have no token and therefore no MCP access.
--
-- SQLite cannot add a UNIQUE column, so uniqueness comes from a separate
-- index; NULL may occur more than once.

ALTER TABLE sandboxes ADD COLUMN mcp_token_hash TEXT
    CHECK (mcp_token_hash IS NULL OR (length(mcp_token_hash) = 64
        AND mcp_token_hash NOT GLOB '*[^0-9a-f]*'));

CREATE UNIQUE INDEX sandboxes_mcp_token_hash ON sandboxes (mcp_token_hash);

-- +goose Down
DROP INDEX sandboxes_mcp_token_hash;
ALTER TABLE sandboxes DROP COLUMN mcp_token_hash;
