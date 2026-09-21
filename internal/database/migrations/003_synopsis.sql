-- The root package's one-line summary, captured at publish time so listings
-- don't have to open every zip.
ALTER TABLE versions ADD COLUMN synopsis TEXT NOT NULL DEFAULT '';
CREATE INDEX versions_published_at ON versions (published_at);
CREATE INDEX modules_created_at ON modules (created_at);
