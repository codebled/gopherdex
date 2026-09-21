-- A module's latest installable release is looked up on nearly every page
-- ("used by" counts, search signals, listings). This index answers it
-- without sorting the module's versions each time.
CREATE INDEX versions_latest ON versions (module_id, published_at DESC, id DESC) WHERE yanked_at IS NULL;

