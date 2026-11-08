-- +goose Up
-- The audit log is append-only (plan section 3.10, CLAUDE.md invariant 6).

-- +goose StatementBegin
CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER audit_events_no_delete;
DROP TRIGGER audit_events_no_update;
