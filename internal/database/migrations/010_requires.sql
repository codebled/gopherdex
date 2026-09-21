-- The module requirements in each published version's go.mod, for
-- "Used by" counts and the dependency graph.
CREATE TABLE version_requires (
    version_id INTEGER NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    version    TEXT NOT NULL,
    indirect   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (version_id, path)
) STRICT;
CREATE INDEX version_requires_path ON version_requires (path);

-- Set once a version's requirements are recorded; older versions are
-- filled in at start-up.
ALTER TABLE versions ADD COLUMN requires_indexed INTEGER NOT NULL DEFAULT 0;
