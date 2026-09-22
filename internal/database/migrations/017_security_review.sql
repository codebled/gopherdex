-- Security review fixes.

-- Removing someone from an organization removes all their access to its
-- modules: team memberships (trigger org_members_leave_teams) and now also
-- direct roles, which a member gets by creating a module or granting
-- themselves one through a team with owner access. People added to one
-- module without being members are outside collaborators and keep theirs.
CREATE TRIGGER org_members_leave_modules AFTER DELETE ON org_members
BEGIN
    DELETE FROM module_roles WHERE user_id = OLD.user_id
        AND module_id IN (SELECT m.id FROM modules m JOIN organizations o ON o.name = m.namespace WHERE o.id = OLD.org_id);
END;

-- Roles are invitations until accepted. Before, an owner could make anyone
-- an owner of an organization or module without asking, then leave: the
-- victim was left as the sole owner vouching for it, and couldn't delete
-- their account. A pending role gives no access and doesn't count as an
-- owner. Everything that exists today counts as accepted.
ALTER TABLE org_members ADD COLUMN accepted_at INTEGER;
ALTER TABLE org_members ADD COLUMN invited_by INTEGER REFERENCES users (id) ON DELETE SET NULL;
UPDATE org_members SET accepted_at = created_at;
ALTER TABLE module_roles ADD COLUMN accepted_at INTEGER;
UPDATE module_roles SET accepted_at = created_at;
