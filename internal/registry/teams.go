package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
)

// Teams group an organization's members and give the group a role on
// chosen modules, e.g. acme/backend maintains acme/api and acme/worker.
// Organization owners manage teams. With member access set to "none", a
// member can only act on the modules their teams (or a direct role) cover.

// Member access settings for an organization.
const (
	MemberAccessMaintainer = "maintainer" // members maintain every module
	MemberAccessNone       = "none"       // members only get what teams grant
)

// Team is a group of an organization's members.
type Team struct {
	ID          int64
	Org         string
	Name        string
	Description string
	Members     int
	Modules     int
	CreatedAt   time.Time
}

// TeamMember is a user in a team.
type TeamMember struct {
	Username string
	Since    time.Time
}

// TeamModule is a module a team has a role on.
type TeamModule struct {
	Path  string
	Role  string
	Since time.Time
}

// TeamAccess is a team's role on a module, for the module's Manage tab.
type TeamAccess struct {
	Team string
	Role string
}

// Team names use the same characters as usernames, but aren't namespaces,
// so words like "security" or "admin" are fine.
var teamName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){0,38}$`)

const maxTeams = 100

// CreateTeam adds a team to an organization.
func (r *Registry) CreateTeam(ctx context.Context, u *accounts.User, org, name, description string, c accounts.Client) error {
	orgID, err := r.requireOrgOwner(ctx, u, org)
	if err != nil {
		return err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	description = strings.TrimSpace(description)
	switch {
	case !teamName.MatchString(name):
		return reject(http.StatusBadRequest, "invalid_team_name", "Use 1 to 39 lower-case letters, digits or single hyphens for the team name, starting and ending with a letter or digit.")
	case len(description) > 300:
		return reject(http.StatusBadRequest, "description_too_long", "Keep the description under 300 characters.")
	}
	var n int
	if err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM teams WHERE org_id = ?`, orgID).Scan(&n); err != nil {
		return err
	}
	if n >= maxTeams {
		return reject(http.StatusConflict, "too_many_teams", "%s already has %d teams. Delete one you no longer use first.", org, maxTeams)
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO teams (org_id, name, description, created_by, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (org_id, name) DO NOTHING`, orgID, name, description, u.ID, r.now().Unix())
	if err != nil {
		return fmt.Errorf("create team %s/%s: %w", org, name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusConflict, "team_exists", "%s already has a team called %s.", org, name)
	}
	if err := r.audit(ctx, tx, u.ID, "team.created", org+"/"+name, c); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteTeam removes a team. Its members lose the access it gave them,
// but stay in the organization.
func (r *Registry) DeleteTeam(ctx context.Context, u *accounts.User, org, name string, c accounts.Client) error {
	teamID, err := r.ownedTeam(ctx, u, org, name)
	if err != nil {
		return err
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM teams WHERE id = ?`, teamID); err != nil {
		return fmt.Errorf("delete team %s/%s: %w", org, name, err)
	}
	if err := r.audit(ctx, tx, u.ID, "team.deleted", org+"/"+name, c); err != nil {
		return err
	}
	return tx.Commit()
}

// Teams lists an organization's teams by name.
func (r *Registry) Teams(ctx context.Context, org string) ([]Team, error) {
	return r.teams(ctx, `WHERE o.name = ? ORDER BY t.name`, org)
}

// TeamByName returns one team, or a 404 Error.
func (r *Registry) TeamByName(ctx context.Context, org, name string) (*Team, error) {
	ts, err := r.teams(ctx, `WHERE o.name = ? AND t.name = ?`, org, strings.ToLower(name))
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, reject(http.StatusNotFound, "unknown_team", "%s has no team called %s.", org, name)
	}
	return &ts[0], nil
}

func (r *Registry) teams(ctx context.Context, where string, args ...any) ([]Team, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT t.id, o.name, t.name, t.description, t.created_at,
			(SELECT COUNT(*) FROM team_members WHERE team_id = t.id),
			(SELECT COUNT(*) FROM team_modules WHERE team_id = t.id)
		FROM teams t JOIN organizations o ON o.id = t.org_id `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		var t Team
		var created int64
		if err := rows.Scan(&t.ID, &t.Org, &t.Name, &t.Description, &created, &t.Members, &t.Modules); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// TeamMembers lists a team's members by username.
func (r *Registry) TeamMembers(ctx context.Context, teamID int64) ([]TeamMember, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT u.username, tm.created_at FROM team_members tm JOIN users u ON u.id = tm.user_id
		WHERE tm.team_id = ? ORDER BY u.username`, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team members: %w", err)
	}
	defer rows.Close()
	out := []TeamMember{}
	for rows.Next() {
		var m TeamMember
		var since int64
		if err := rows.Scan(&m.Username, &since); err != nil {
			return nil, err
		}
		m.Since = time.Unix(since, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// TeamModules lists the modules a team has a role on, by path.
func (r *Registry) TeamModules(ctx context.Context, teamID int64) ([]TeamModule, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT m.path, tm.role, tm.created_at FROM team_modules tm JOIN modules m ON m.id = tm.module_id
		WHERE tm.team_id = ? ORDER BY m.path`, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team modules: %w", err)
	}
	defer rows.Close()
	out := []TeamModule{}
	for rows.Next() {
		var m TeamModule
		var since int64
		if err := rows.Scan(&m.Path, &m.Role, &since); err != nil {
			return nil, err
		}
		m.Since = time.Unix(since, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// ModuleTeams lists the teams with a role on a module, owners first.
func (r *Registry) ModuleTeams(ctx context.Context, modPath string) ([]TeamAccess, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT t.name, tm.role FROM team_modules tm JOIN teams t ON t.id = tm.team_id
		JOIN modules m ON m.id = tm.module_id WHERE m.path = ? ORDER BY tm.role = 'owner' DESC, t.name`, modPath)
	if err != nil {
		return nil, fmt.Errorf("list teams of %s: %w", modPath, err)
	}
	defer rows.Close()
	out := []TeamAccess{}
	for rows.Next() {
		var a TeamAccess
		if err := rows.Scan(&a.Team, &a.Role); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddTeamMember adds an organization member to a team.
func (r *Registry) AddTeamMember(ctx context.Context, u *accounts.User, org, team, username string, c accounts.Client) error {
	teamID, err := r.ownedTeam(ctx, u, org, team)
	if err != nil {
		return err
	}
	username = accounts.NormalizeUsername(strings.TrimPrefix(strings.TrimSpace(username), "@"))
	var userID int64
	var orgRole sql.NullString
	err = r.DB.QueryRowContext(ctx, `SELECT u.id, (SELECT om.role FROM org_members om JOIN organizations o ON o.id = om.org_id
			WHERE o.name = ? AND om.user_id = u.id AND om.accepted_at IS NOT NULL)
		FROM users u WHERE u.username = ?`, org, username).Scan(&userID, &orgRole)
	if errors.Is(err, sql.ErrNoRows) {
		return reject(http.StatusNotFound, "unknown_user", "There's no user @%s.", username)
	}
	if err != nil {
		return err
	}
	if !orgRole.Valid {
		return reject(http.StatusConflict, "not_an_org_member", "@%s isn't a member of %s. Add them to the organization first.", username, org)
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO team_members (team_id, user_id, created_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		teamID, userID, r.now().Unix()); err != nil {
		return fmt.Errorf("add @%s to %s/%s: %w", username, org, team, err)
	}
	if err := r.audit(ctx, tx, u.ID, "team.member_added", fmt.Sprintf("%s/%s @%s", org, team, username), c); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveTeamMember takes someone out of a team. They stay in the
// organization.
func (r *Registry) RemoveTeamMember(ctx context.Context, u *accounts.User, org, team, username string, c accounts.Client) error {
	teamID, err := r.ownedTeam(ctx, u, org, team)
	if err != nil {
		return err
	}
	username = accounts.NormalizeUsername(strings.TrimPrefix(username, "@"))
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM team_members WHERE team_id = ? AND user_id = (SELECT id FROM users WHERE username = ?)`, teamID, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "not_a_team_member", "@%s isn't in %s/%s.", username, org, team)
	}
	if err := r.audit(ctx, tx, u.ID, "team.member_removed", fmt.Sprintf("%s/%s @%s", org, team, username), c); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTeamModule gives a team a role on one of the organization's modules,
// or changes it.
func (r *Registry) SetTeamModule(ctx context.Context, u *accounts.User, org, team, modPath, roleName string, c accounts.Client) error {
	role, ok := parseRole(roleName)
	if !ok {
		return reject(http.StatusBadRequest, "invalid_role", "Choose owner or maintainer.")
	}
	teamID, err := r.ownedTeam(ctx, u, org, team)
	if err != nil {
		return err
	}
	modPath = strings.TrimSpace(modPath)
	if !strings.Contains(modPath, "/") {
		modPath = r.ModuleHost + "/" + org + "/" + modPath // "api" for gopherdex.dev/acme/api
	}
	var moduleID int64
	var namespace string
	err = r.DB.QueryRowContext(ctx, `SELECT id, namespace FROM modules WHERE path = ?`, modPath).Scan(&moduleID, &namespace)
	if errors.Is(err, sql.ErrNoRows) {
		return reject(http.StatusNotFound, "unknown_module", "There's no module %s. Publish its first version, then give teams access.", modPath)
	}
	if err != nil {
		return err
	}
	if namespace != org {
		return reject(http.StatusForbidden, "other_namespace", "%s isn't one of %s's modules. Teams only get access to modules under %s/%s/.", modPath, org, r.ModuleHost, org)
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO team_modules (team_id, module_id, role, created_by, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (team_id, module_id) DO UPDATE SET role = excluded.role`, teamID, moduleID, role.String(), u.ID, r.now().Unix()); err != nil {
		return fmt.Errorf("give %s/%s access to %s: %w", org, team, modPath, err)
	}
	if err := r.audit(ctx, tx, u.ID, "team.module_set", fmt.Sprintf("%s/%s %s %s", org, team, modPath, role), c); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveTeamModule takes away a team's role on a module.
func (r *Registry) RemoveTeamModule(ctx context.Context, u *accounts.User, org, team, modPath string, c accounts.Client) error {
	teamID, err := r.ownedTeam(ctx, u, org, team)
	if err != nil {
		return err
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM team_modules WHERE team_id = ? AND module_id = (SELECT id FROM modules WHERE path = ?)`, teamID, modPath)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "no_team_access", "%s/%s has no access to %s.", org, team, modPath)
	}
	if err := r.audit(ctx, tx, u.ID, "team.module_removed", fmt.Sprintf("%s/%s %s", org, team, modPath), c); err != nil {
		return err
	}
	return tx.Commit()
}

// SetMemberAccess sets what members may do without a team: maintain every
// module ("maintainer"), or nothing ("none").
func (r *Registry) SetMemberAccess(ctx context.Context, u *accounts.User, org, access string, c accounts.Client) error {
	if access != MemberAccessMaintainer && access != MemberAccessNone {
		return reject(http.StatusBadRequest, "invalid_access", "Choose maintainer or none.")
	}
	orgID, err := r.requireOrgOwner(ctx, u, org)
	if err != nil {
		return err
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE organizations SET member_access = ? WHERE id = ?`, access, orgID); err != nil {
		return err
	}
	if err := r.audit(ctx, tx, u.ID, "org.member_access", org+" "+access, c); err != nil {
		return err
	}
	return tx.Commit()
}

// IsOrgMember reports whether u belongs to the organization, as owner or
// member. Teams are only shown to members.
func (r *Registry) IsOrgMember(ctx context.Context, u *accounts.User, org string) (bool, error) {
	if u == nil {
		return false, nil
	}
	role, err := r.orgMember(ctx, org, u.ID)
	return role != "", err
}

// ownedTeam returns the team's ID if u owns its organization.
func (r *Registry) ownedTeam(ctx context.Context, u *accounts.User, org, name string) (int64, error) {
	orgID, err := r.requireOrgOwner(ctx, u, org)
	if err != nil {
		return 0, err
	}
	var id int64
	err = r.DB.QueryRowContext(ctx, `SELECT id FROM teams WHERE org_id = ? AND name = ?`, orgID, strings.ToLower(name)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, reject(http.StatusNotFound, "unknown_team", "%s has no team called %s.", org, name)
	}
	return id, err
}
