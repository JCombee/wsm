-- +goose Up
-- M2a: a project's image comes from a public registry (image_ref) or is
-- built from a Containerfile kept only in this database (CLAUDE.md
-- invariant 5). Existing projects use a registry image.
--
-- A registry project has an image_ref and no Containerfile; a Containerfile
-- project has a Containerfile of at most 256 KiB and an empty image_ref,
-- since wsm derives its tag from the project ID and the Containerfile.

ALTER TABLE projects ADD COLUMN image_source TEXT NOT NULL DEFAULT 'registry'
    CHECK (image_source IN ('registry', 'containerfile'));

ALTER TABLE projects ADD COLUMN containerfile TEXT
    CHECK (
        (image_source = 'registry' AND containerfile IS NULL AND image_ref <> '')
        OR (image_source = 'containerfile' AND containerfile IS NOT NULL AND image_ref = ''
            AND length(CAST(containerfile AS BLOB)) BETWEEN 1 AND 262144)
    );

-- +goose Down
ALTER TABLE projects DROP COLUMN containerfile;
ALTER TABLE projects DROP COLUMN image_source;
