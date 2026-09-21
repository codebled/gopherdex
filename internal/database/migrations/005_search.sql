-- Details extracted from each version's zip when it is published, used for
-- search and filters. indexed = 0 marks versions published before this
-- migration; the server fills them in at start-up.
ALTER TABLE versions ADD COLUMN readme_text TEXT NOT NULL DEFAULT '';
ALTER TABLE versions ADD COLUMN license TEXT NOT NULL DEFAULT '';     -- SPDX IDs, comma-separated
ALTER TABLE versions ADD COLUMN go_version TEXT NOT NULL DEFAULT '';  -- go directive, e.g. 1.22
ALTER TABLE versions ADD COLUMN indexed INTEGER NOT NULL DEFAULT 0;

-- One row per searchable module, describing its latest installable version.
-- Modules with every version yanked are left out.
CREATE TABLE module_meta (
    module_id    INTEGER PRIMARY KEY REFERENCES modules (id) ON DELETE CASCADE,
    path         TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    version      TEXT NOT NULL,
    synopsis     TEXT NOT NULL,
    license      TEXT NOT NULL,
    go_version   TEXT NOT NULL,
    go_num       INTEGER NOT NULL,  -- 1.22 → 1022, 0 when unknown
    published_at INTEGER NOT NULL,  -- of version
    created_at   INTEGER NOT NULL,  -- of the module
    deprecated   INTEGER NOT NULL
) STRICT;
CREATE INDEX module_meta_published_at ON module_meta (published_at);
CREATE INDEX module_meta_license ON module_meta (license);

-- Full-text index; rowid = module id.
CREATE VIRTUAL TABLE module_fts USING fts5(
    path, name, synopsis, readme,
    tokenize = 'unicode61 remove_diacritics 2',
    prefix = '2 3'
);

-- Module zips served by this registry's proxy, per version per day.
CREATE TABLE downloads (
    module_id INTEGER NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    version   TEXT NOT NULL,
    day       INTEGER NOT NULL,  -- days since the Unix epoch, UTC
    count     INTEGER NOT NULL,
    PRIMARY KEY (module_id, version, day)
) STRICT, WITHOUT ROWID;
CREATE INDEX downloads_day ON downloads (day);
