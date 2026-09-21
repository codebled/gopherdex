-- Password reset links, stored hashed like every other secret.
CREATE TABLE password_resets (
    token_hash BLOB PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
) STRICT;
CREATE INDEX password_resets_user_id ON password_resets (user_id);

-- Two-factor authentication with an authenticator app (TOTP, RFC 6238).
ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';       -- base32; set during setup
ALTER TABLE users ADD COLUMN totp_enabled_at INTEGER;                     -- NULL until confirmed
ALTER TABLE users ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0;   -- blocks code reuse

CREATE TABLE recovery_codes (
    user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash BLOB NOT NULL,
    used_at   INTEGER,
    PRIMARY KEY (user_id, code_hash)
) STRICT;

-- The step between a correct password and a correct 2FA code.
CREATE TABLE login_challenges (
    token_hash BLOB PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Reports from users about harmful modules, reviewed by admins.
CREATE TABLE reports (
    id          INTEGER PRIMARY KEY,
    module_id   INTEGER NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    reporter_id INTEGER REFERENCES users (id) ON DELETE SET NULL,
    category    TEXT NOT NULL CHECK (category IN ('malware', 'typosquatting', 'spam', 'license', 'other')),
    details     TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'dismissed', 'actioned')),
    created_at  INTEGER NOT NULL,
    resolved_at INTEGER,
    resolved_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    resolution  TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX reports_status ON reports (status, created_at);

-- A quarantined module is hidden from pages, search and the proxy while
-- admins review it.
ALTER TABLE modules ADD COLUMN quarantined_at INTEGER;
ALTER TABLE modules ADD COLUMN quarantine_reason TEXT NOT NULL DEFAULT '';
