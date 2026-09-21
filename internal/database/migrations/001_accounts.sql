-- Namespaces are the <owner> segment of gopherdex.dev/<owner>/<module>.
-- Users and (later) organizations share one namespace so names never collide.
CREATE TABLE namespaces (
    name       TEXT PRIMARY KEY,               -- lower-case
    kind       TEXT NOT NULL CHECK (kind IN ('user', 'org')),
    created_at INTEGER NOT NULL                -- unix seconds
) STRICT;

CREATE TABLE users (
    id                INTEGER PRIMARY KEY,
    username          TEXT NOT NULL UNIQUE REFERENCES namespaces (name),
    email             TEXT NOT NULL UNIQUE,     -- lower-case
    password_hash     TEXT NOT NULL,            -- argon2id PHC string
    email_verified_at INTEGER,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
) STRICT;

-- Only a SHA-256 of each secret is stored, so a leaked database cannot be
-- used to sign in or publish.
CREATE TABLE sessions (
    token_hash   BLOB PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    ip           TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX sessions_user_id ON sessions (user_id);

CREATE TABLE email_verifications (
    token_hash BLOB PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email      TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
) STRICT;
CREATE INDEX email_verifications_user_id ON email_verifications (user_id);

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   BLOB NOT NULL UNIQUE,
    prefix       TEXT NOT NULL,                 -- first characters, shown in the UI
    scope        TEXT NOT NULL,                 -- e.g. "namespace:alice"
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    last_used_at INTEGER,
    revoked_at   INTEGER
) STRICT;
CREATE INDEX api_tokens_user_id ON api_tokens (user_id);

CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER REFERENCES users (id) ON DELETE SET NULL,
    action     TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX audit_log_user_id ON audit_log (user_id, created_at);
