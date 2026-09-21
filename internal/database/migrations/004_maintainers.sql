-- Who may maintain a module. Owners can do everything; maintainers can
-- publish and yank. Modules in an organization's namespace are also managed
-- by the organization (org owners act as owners, members as maintainers).
CREATE TABLE module_roles (
    module_id  INTEGER NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'maintainer')),
    created_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (module_id, user_id)
) STRICT;
CREATE INDEX module_roles_user_id ON module_roles (user_id);

-- Until now a module was owned by the user whose namespace it lives in.
INSERT INTO module_roles (module_id, user_id, role, created_by, created_at)
SELECT m.id, u.id, 'owner', u.id, m.created_at FROM modules m JOIN users u ON u.username = m.namespace;

CREATE TABLE organizations (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE REFERENCES namespaces (name),
    display_name TEXT NOT NULL DEFAULT '',
    created_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE org_members (
    org_id     INTEGER NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (org_id, user_id)
) STRICT;
CREATE INDEX org_members_user_id ON org_members (user_id);

-- Yanked versions stay downloadable (pinned builds keep working) but are
-- left out of version lists and @latest.
ALTER TABLE versions ADD COLUMN yanked_at INTEGER;
ALTER TABLE versions ADD COLUMN yank_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE versions ADD COLUMN yanked_by INTEGER REFERENCES users (id) ON DELETE SET NULL;

-- A deprecation notice set on the website, without publishing a release.
ALTER TABLE modules ADD COLUMN deprecation TEXT NOT NULL DEFAULT '';
ALTER TABLE modules ADD COLUMN successor TEXT NOT NULL DEFAULT '';
ALTER TABLE modules ADD COLUMN deprecated_at INTEGER;
