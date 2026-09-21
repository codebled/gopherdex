-- Search at scale (found by load testing 100,000 modules).

-- Downloads in the last 30 days, kept per module so search can sort and
-- break ties by popularity without adding up every match's downloads on
-- each query. Updated when download counts are written, and recomputed
-- for everyone once a day as old days leave the window.
ALTER TABLE module_meta ADD COLUMN downloads_30d INTEGER NOT NULL DEFAULT 0;
CREATE INDEX module_meta_downloads ON module_meta (downloads_30d DESC);
UPDATE module_meta SET downloads_30d = COALESCE((SELECT SUM(d.count) FROM downloads d
    WHERE d.module_id = module_meta.module_id AND d.day > CAST(strftime('%s', 'now') AS INTEGER) / 86400 - 30), 0);

-- Every module path starts with the registry's host, so indexing it made a
-- query like "go" or "dev" match every module. Index paths without it.
UPDATE module_fts SET path = substr(path, instr(path, '/') + 1) WHERE instr(path, '/') > 0;
