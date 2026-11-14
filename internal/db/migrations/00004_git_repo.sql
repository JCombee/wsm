-- +goose Up
-- M2b: a project can have a git repository, reached over SSH with a
-- per-project deploy key (ADR 0009). Its sandboxes start with the base
-- branch on their own branch agent/<sandbox> (ADR 0010).
--
-- A project without a repository has none of the four columns. A project
-- with one has all four. The private key is stored only sealed with
-- AES-256-GCM, bound to the project ID.

ALTER TABLE projects ADD COLUMN repo_url TEXT
    CHECK (repo_url IS NULL OR length(repo_url) BETWEEN 1 AND 512);
ALTER TABLE projects ADD COLUMN base_branch TEXT
    CHECK ((repo_url IS NULL) = (base_branch IS NULL)
        AND (base_branch IS NULL OR length(base_branch) BETWEEN 1 AND 200));
ALTER TABLE projects ADD COLUMN deploy_key_pub TEXT
    CHECK ((repo_url IS NULL) = (deploy_key_pub IS NULL));
ALTER TABLE projects ADD COLUMN deploy_key_priv_enc BLOB
    CHECK ((repo_url IS NULL) = (deploy_key_priv_enc IS NULL));

-- branch is agent/<slug> for a sandbox of a project with a repository.
-- seeded_at is set once the code is in the sandbox's /workspace.
ALTER TABLE sandboxes ADD COLUMN branch TEXT;
ALTER TABLE sandboxes ADD COLUMN seeded_at TEXT;

-- +goose Down
ALTER TABLE sandboxes DROP COLUMN seeded_at;
ALTER TABLE sandboxes DROP COLUMN branch;
ALTER TABLE projects DROP COLUMN deploy_key_priv_enc;
ALTER TABLE projects DROP COLUMN deploy_key_pub;
ALTER TABLE projects DROP COLUMN base_branch;
ALTER TABLE projects DROP COLUMN repo_url;
