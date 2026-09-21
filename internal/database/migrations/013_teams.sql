-- What organization members may do with the organization's modules without
-- a team: 'maintainer' (publish and yank every module, as before teams
-- existed) or 'none' (only what their teams grant).
ALTER TABLE organizations ADD COLUMN member_access TEXT NOT NULL DEFAULT 'maintainer'
    CHECK (member_access IN ('maintainer', 'none'));

-- A group of an organization's members, e.g. acme/backend.
CREATE TABLE teams (
    id          INTEGER PRIMARY KEY,
    org_id      INTEGER NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_by  INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (org_id, name)
) STRICT;

CREATE TABLE team_members (
    team_id    INTEGER NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (team_id, user_id)
) STRICT;
CREATE INDEX team_members_user_id ON team_members (user_id);

-- A team's role on one of the organization's modules.
CREATE TABLE team_modules (
    team_id    INTEGER NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    module_id  INTEGER NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'maintainer')),
    created_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (team_id, module_id)
) STRICT;
CREATE INDEX team_modules_module_id ON team_modules (module_id);

-- Leaving the organization means leaving its teams.
CREATE TRIGGER org_members_leave_teams AFTER DELETE ON org_members
BEGIN
    DELETE FROM team_members WHERE user_id = OLD.user_id
        AND team_id IN (SELECT id FROM teams WHERE org_id = OLD.org_id);
END;
