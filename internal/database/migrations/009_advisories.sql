-- Security advisories that module owners publish for their own modules.
-- They're served as OSV entries in the Go vulnerability database format
-- at /vulndb, so govulncheck can check against them.
CREATE TABLE advisories (
    id          INTEGER PRIMARY KEY,
    advisory_id TEXT NOT NULL UNIQUE,            -- e.g. GDX-2026-0001
    module_id   INTEGER NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    summary     TEXT NOT NULL,
    details     TEXT NOT NULL,
    aliases     TEXT NOT NULL DEFAULT '[]',      -- JSON: ["CVE-…", "GHSA-…"]
    ranges      TEXT NOT NULL,                   -- JSON: [{"introduced": "v1.0.0", "fixed": "v1.2.3"}]; "" = from the start / no fix
    packages    TEXT NOT NULL DEFAULT '[]',      -- JSON: [{"path": "…", "symbols": ["Func", "Type.Method"]}]
    refs        TEXT NOT NULL DEFAULT '[]',      -- JSON: ["https://…"]
    credits     TEXT NOT NULL DEFAULT '',
    created_by  INTEGER REFERENCES users (id) ON DELETE SET NULL,
    published_at INTEGER NOT NULL,
    modified_at INTEGER NOT NULL,
    withdrawn_at INTEGER
) STRICT;
CREATE INDEX advisories_module ON advisories (module_id);
