-- Passkeys (WebAuthn): sign in with Touch ID, Face ID, Windows Hello or a
-- security key, and use them as a second factor.
ALTER TABLE users ADD COLUMN webauthn_handle BLOB; -- random user handle given to authenticators, set on first passkey
CREATE UNIQUE INDEX users_webauthn_handle ON users (webauthn_handle) WHERE webauthn_handle IS NOT NULL;

CREATE TABLE passkeys (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id BLOB NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    credential    TEXT NOT NULL,     -- JSON: public key, sign count, flags (go-webauthn Credential)
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER
) STRICT;
CREATE INDEX passkeys_user_id ON passkeys (user_id);

-- A registration or sign-in in progress: the challenge sent to the browser,
-- redeemed once by the response.
CREATE TABLE webauthn_ceremonies (
    token_hash BLOB PRIMARY KEY,
    user_id    INTEGER REFERENCES users (id) ON DELETE CASCADE, -- NULL for passkey sign-in, where the user isn't known yet
    kind       TEXT NOT NULL CHECK (kind IN ('register', 'login', 'second-factor')),
    session    TEXT NOT NULL,        -- JSON go-webauthn SessionData
    expires_at INTEGER NOT NULL
) STRICT;
