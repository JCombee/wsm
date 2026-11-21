-- +goose Up
-- M5: previews (ADR 0017). app_port is the port the project's app listens
-- on inside the pod; NULL means the project has no preview. host_port is
-- where a sandbox's pod publishes that port on 127.0.0.1; only the manager
-- connects to it (plan section 3.3).

ALTER TABLE projects ADD COLUMN app_port INTEGER CHECK (app_port BETWEEN 1 AND 65535);
ALTER TABLE sandboxes ADD COLUMN host_port INTEGER CHECK (host_port BETWEEN 1024 AND 65535);
CREATE UNIQUE INDEX sandboxes_host_port ON sandboxes (host_port) WHERE host_port IS NOT NULL;

-- +goose Down
DROP INDEX sandboxes_host_port;
ALTER TABLE sandboxes DROP COLUMN host_port;
ALTER TABLE projects DROP COLUMN app_port;
