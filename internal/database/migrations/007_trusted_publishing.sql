-- Trusted publishers let a CI workflow publish a module without a stored
-- API token: the workflow proves who it is with an OIDC ID token and gets
-- a short-lived token for that one module in exchange.
--
-- module_path may name a module that doesn't exist yet (a "pending"
-- publisher); its first trusted publish creates it.
CREATE TABLE trusted_publishers (
    id                  INTEGER PRIMARY KEY,
    module_path         TEXT NOT NULL,
    provider            TEXT NOT NULL CHECK (provider IN ('github')),
    repository          TEXT NOT NULL,             -- "owner/name", lower-case
    workflow            TEXT NOT NULL,             -- file name, e.g. "release.yml"
    environment         TEXT NOT NULL DEFAULT '',  -- '' means any environment
    -- GitHub's numeric ID for the repository owner, recorded on first use,
    -- so if the account is deleted and someone re-registers its name, their
    -- workflows can't publish.
    repository_owner_id TEXT NOT NULL DEFAULT '',
    created_by          INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at          INTEGER NOT NULL,
    last_used_at        INTEGER,
    UNIQUE (module_path, provider, repository, workflow, environment)
) STRICT;
CREATE INDEX trusted_publishers_created_by ON trusted_publishers (created_by);

-- Each ID token is exchanged once.
CREATE TABLE oidc_token_uses (
    jti        TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL
) STRICT;

-- Tokens minted for a trusted publisher, and the verified claims that
-- earned them. The claims become the provenance of what they publish.
ALTER TABLE api_tokens ADD COLUMN publisher_id INTEGER REFERENCES trusted_publishers (id) ON DELETE SET NULL;
ALTER TABLE api_tokens ADD COLUMN claims TEXT NOT NULL DEFAULT '';

ALTER TABLE versions ADD COLUMN provenance TEXT NOT NULL DEFAULT '';  -- JSON; '' for token uploads
