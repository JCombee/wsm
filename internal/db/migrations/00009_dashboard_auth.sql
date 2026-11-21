-- +goose Up
-- M4: dashboard login with passkeys (ADR 0016) and preview access (ADR 0017).
-- Tokens and session IDs are never stored, only their SHA-256 as lowercase
-- hex.

CREATE TABLE users (
    id          TEXT PRIMARY KEY,
    -- The WebAuthn user handle: 32 random bytes, never shown.
    handle      BLOB NOT NULL UNIQUE CHECK (length(handle) = 32),
    name        TEXT NOT NULL UNIQUE,
    created_at  TEXT NOT NULL
) STRICT;

-- credential_json is go-webauthn's Credential (public key, sign count,
-- flags, transports); credential_id is copied out of it for the lookup.
CREATE TABLE passkeys (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id   BLOB NOT NULL UNIQUE,
    credential_json TEXT NOT NULL,
    name            TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    last_used_at    TEXT
) STRICT;

CREATE TABLE sessions (
    id_hash      TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    user_agent   TEXT NOT NULL
) STRICT;

-- One-time registration links from "wsm auth enroll".
CREATE TABLE enroll_tokens (
    token_hash  TEXT PRIMARY KEY,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
) STRICT;

-- One-time tokens that hand access from the dashboard to one preview host.
CREATE TABLE preview_tokens (
    token_hash  TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    host        TEXT NOT NULL,
    expires_at  TEXT NOT NULL
) STRICT;

-- Sessions on one preview host (cookie __Host-wsm_preview).
CREATE TABLE preview_sessions (
    id_hash     TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    host        TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE preview_sessions;
DROP TABLE preview_tokens;
DROP TABLE enroll_tokens;
DROP TABLE sessions;
DROP TABLE passkeys;
DROP TABLE users;
