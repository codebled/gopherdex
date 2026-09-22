-- A namespace's feed lists its newest releases. With thousands of modules
-- in a namespace, finding them meant reading and sorting every release of
-- every module (55,000 rows for the largest namespace in load testing).
-- A module's namespace never changes, so each release records it, and an
-- index gives the newest releases of a namespace directly.
ALTER TABLE versions ADD COLUMN namespace TEXT NOT NULL DEFAULT '';
UPDATE versions SET namespace = (SELECT namespace FROM modules WHERE modules.id = versions.module_id);
CREATE INDEX versions_namespace_recent ON versions (namespace, published_at DESC, id DESC) WHERE yanked_at IS NULL;
