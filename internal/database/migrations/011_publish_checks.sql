-- What the publish-time checks noticed about each version. Blocking
-- findings stop the upload, so only warnings are stored.
CREATE TABLE publish_findings (
    id         INTEGER PRIMARY KEY,
    version_id INTEGER NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    rule       TEXT NOT NULL,
    severity   TEXT NOT NULL,
    file       TEXT NOT NULL DEFAULT '',
    line       INTEGER NOT NULL DEFAULT 0,
    message    TEXT NOT NULL
) STRICT;
CREATE INDEX publish_findings_version ON publish_findings (version_id);
