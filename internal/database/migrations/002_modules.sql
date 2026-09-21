-- A module is one module path, e.g. gopherdex.dev/alice/retry. A new major
-- version (…/retry/v2) is a separate module, exactly as the go command sees it.
CREATE TABLE modules (
    id         INTEGER PRIMARY KEY,
    path       TEXT NOT NULL UNIQUE,
    namespace  TEXT NOT NULL REFERENCES namespaces (name),
    created_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX modules_namespace ON modules (namespace);

-- Published versions never change. Retrying an identical upload is a no-op;
-- different content for an existing version is refused.
CREATE TABLE versions (
    id           INTEGER PRIMARY KEY,
    module_id    INTEGER NOT NULL REFERENCES modules (id),
    version      TEXT NOT NULL,
    go_mod       BLOB NOT NULL,
    zip_key      TEXT NOT NULL,                -- blob store key
    zip_size     INTEGER NOT NULL,
    zip_sha256   TEXT NOT NULL,
    h1           TEXT NOT NULL,                -- go.sum hash of the module zip
    go_mod_h1    TEXT NOT NULL,                -- go.sum hash of go.mod
    vcs          TEXT NOT NULL DEFAULT '',
    repository   TEXT NOT NULL DEFAULT '',
    commit_hash  TEXT NOT NULL DEFAULT '',
    ref          TEXT NOT NULL DEFAULT '',
    published_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    token_id     INTEGER REFERENCES api_tokens (id) ON DELETE SET NULL,
    published_at INTEGER NOT NULL,
    UNIQUE (module_id, version)
) STRICT;
