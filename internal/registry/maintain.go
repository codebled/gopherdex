package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

// Role is what a user may do with a module.
type Role int

const (
	RoleNone       Role = iota
	RoleMaintainer      // publish and yank
	RoleOwner           // also deprecate and manage who has access
)

func (r Role) String() string {
	switch r {
	case RoleOwner:
		return "owner"
	case RoleMaintainer:
		return "maintainer"
	}
	return "none"
}

func parseRole(s string) (Role, bool) {
	switch s {
	case "owner":
		return RoleOwner, true
	case "maintainer":
		return RoleMaintainer, true
	}
	return RoleNone, false
}

const maxReason = 500

// Role returns u's role on an existing module: the stronger of their
// module role and, for organization modules, their organization role (org
// owners act as owners, members as maintainers).
func (r *Registry) Role(ctx context.Context, u *accounts.User, modPath string) (Role, error) {
	if u == nil {
		return RoleNone, nil
	}
	var moduleRole, orgRole sql.NullString
	err := r.DB.QueryRowContext(ctx, `SELECT
			(SELECT mr.role FROM module_roles mr WHERE mr.module_id = m.id AND mr.user_id = ?),
			(SELECT om.role FROM organizations o JOIN org_members om ON om.org_id = o.id WHERE o.name = m.namespace AND om.user_id = ?)
		FROM modules m WHERE m.path = ?`, u.ID, u.ID, modPath).Scan(&moduleRole, &orgRole)
	if errors.Is(err, sql.ErrNoRows) {
		return RoleNone, fmt.Errorf("module %s: %w", modPath, module.ErrNotFound)
	}
	if err != nil {
		return RoleNone, fmt.Errorf("look up role on %s: %w", modPath, err)
	}
	role := RoleNone
	if rl, ok := parseRole(moduleRole.String); ok {
		role = rl
	}
	switch orgRole.String {
	case "owner":
		role = RoleOwner
	case "member":
		role = max(role, RoleMaintainer)
	}
	return role, nil
}

// canPublish decides whether u may publish modPath: maintainers and owners
// of an existing module, or, for a new module, the namespace's user or a
// member of the namespace's organization.
func (r *Registry) canPublish(ctx context.Context, u *accounts.User, modPath, namespace string) error {
	role, err := r.Role(ctx, u, modPath)
	switch {
	case err == nil && role >= RoleMaintainer:
		return nil
	case err == nil:
		return reject(http.StatusForbidden, "forbidden_module",
			"@%s can't publish %s. Ask one of its owners to add you as a maintainer.", u.Username, modPath)
	case !errors.Is(err, module.ErrNotFound):
		return err
	}
	if namespace == u.Username {
		return nil
	}
	isMember, err := r.orgMember(ctx, namespace, u.ID)
	if err != nil {
		return err
	}
	if isMember != "" {
		return nil
	}
	return reject(http.StatusForbidden, "forbidden_namespace",
		"@%s can't create modules in %s/%s/. Publish under your own namespace, %s/%s/, or ask an owner of @%s to add you to the organization.",
		u.Username, r.ModuleHost, namespace, r.ModuleHost, u.Username, namespace)
}

func (r *Registry) requireRole(ctx context.Context, u *accounts.User, modPath string, want Role) (int64, error) {
	role, err := r.Role(ctx, u, modPath)
	if err != nil {
		return 0, err
	}
	if role < want {
		article := "a"
		if want == RoleOwner {
			article = "an"
		}
		return 0, reject(http.StatusForbidden, "forbidden", "You need to be %s %s of %s to do that.", article, want, modPath)
	}
	var id int64
	if err := r.DB.QueryRowContext(ctx, `SELECT id FROM modules WHERE path = ?`, modPath).Scan(&id); err != nil {
		return 0, fmt.Errorf("look up %s: %w", modPath, err)
	}
	return id, nil
}

func (r *Registry) audit(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, userID int64, action, detail string, c accounts.Client) error {
	_, err := db.ExecContext(ctx, `INSERT INTO audit_log (user_id, action, detail, ip, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, action, detail, c.IP, r.now().Unix())
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// ---- Yanking ----

// Yank hides a version from version lists and @latest. The files stay
// downloadable, so builds that already pinned the version keep working.
func (r *Registry) Yank(ctx context.Context, u *accounts.User, modPath, version, reason string, c accounts.Client) error {
	return r.setYanked(ctx, u, modPath, version, strings.TrimSpace(reason), true, c)
}

// Unyank makes a yanked version installable again.
func (r *Registry) Unyank(ctx context.Context, u *accounts.User, modPath, version string, c accounts.Client) error {
	return r.setYanked(ctx, u, modPath, version, "", false, c)
}

func (r *Registry) setYanked(ctx context.Context, u *accounts.User, modPath, version, reason string, yank bool, c accounts.Client) error {
	if len(reason) > maxReason {
		return reject(http.StatusBadRequest, "reason_too_long", "Keep the reason under %d characters.", maxReason)
	}
	moduleID, err := r.requireRole(ctx, u, modPath, RoleMaintainer)
	if err != nil {
		return err
	}
	var res sql.Result
	if yank {
		res, err = r.DB.ExecContext(ctx, `UPDATE versions SET yanked_at = ?, yank_reason = ?, yanked_by = ?
			WHERE module_id = ? AND version = ? AND yanked_at IS NULL`, r.now().Unix(), reason, u.ID, moduleID, version)
	} else {
		res, err = r.DB.ExecContext(ctx, `UPDATE versions SET yanked_at = NULL, yank_reason = '', yanked_by = NULL
			WHERE module_id = ? AND version = ? AND yanked_at IS NOT NULL`, moduleID, version)
	}
	if err != nil {
		return fmt.Errorf("update %s@%s: %w", modPath, version, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions WHERE module_id = ? AND version = ?`, moduleID, version).Scan(&exists)
		if exists == 0 {
			return fmt.Errorf("%s@%s: %w", modPath, version, module.ErrNotFound)
		}
		return nil // already in the requested state
	}
	action := "version.unyanked"
	if yank {
		action = "version.yanked"
	}
	r.reindexPath(ctx, modPath)
	return r.audit(ctx, r.DB, u.ID, action, fmt.Sprintf("%s@%s %s", modPath, version, reason), c)
}

// ---- Deprecation ----

// Deprecate marks a module deprecated on the registry, optionally naming
// the module to use instead. The go command only sees deprecations written
// in go.mod; this one shows on the website and in search.
func (r *Registry) Deprecate(ctx context.Context, u *accounts.User, modPath, message, successor string, c accounts.Client) error {
	message, successor = strings.TrimSpace(message), strings.TrimSpace(successor)
	if message == "" {
		return reject(http.StatusBadRequest, "missing_message", "Say why the module is deprecated, so users know what to do.")
	}
	if len(message) > maxReason {
		return reject(http.StatusBadRequest, "reason_too_long", "Keep the message under %d characters.", maxReason)
	}
	if successor != "" {
		if err := module.CheckPath(successor); err != nil || successor == modPath {
			return reject(http.StatusBadRequest, "invalid_successor", "The replacement must be a different Go module path, like %s/you/newname.", r.ModuleHost)
		}
	}
	moduleID, err := r.requireRole(ctx, u, modPath, RoleOwner)
	if err != nil {
		return err
	}
	if _, err := r.DB.ExecContext(ctx, `UPDATE modules SET deprecation = ?, successor = ?, deprecated_at = ? WHERE id = ?`,
		message, successor, r.now().Unix(), moduleID); err != nil {
		return fmt.Errorf("deprecate %s: %w", modPath, err)
	}
	r.reindexPath(ctx, modPath)
	return r.audit(ctx, r.DB, u.ID, "module.deprecated", modPath+" "+message, c)
}

// Undeprecate removes the registry deprecation notice.
func (r *Registry) Undeprecate(ctx context.Context, u *accounts.User, modPath string, c accounts.Client) error {
	moduleID, err := r.requireRole(ctx, u, modPath, RoleOwner)
	if err != nil {
		return err
	}
	if _, err := r.DB.ExecContext(ctx, `UPDATE modules SET deprecation = '', successor = '', deprecated_at = NULL WHERE id = ?`, moduleID); err != nil {
		return fmt.Errorf("undeprecate %s: %w", modPath, err)
	}
	r.reindexPath(ctx, modPath)
	return r.audit(ctx, r.DB, u.ID, "module.undeprecated", modPath, c)
}

// ---- Collaborators ----

// Collaborator is a user with a role on a module.
type Collaborator struct {
	Username string
	Role     string
	Since    time.Time
}

// Collaborators lists a module's owners and maintainers, owners first.
func (r *Registry) Collaborators(ctx context.Context, modPath string) ([]Collaborator, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT u.username, mr.role, mr.created_at FROM module_roles mr
		JOIN modules m ON m.id = mr.module_id JOIN users u ON u.id = mr.user_id
		WHERE m.path = ? ORDER BY mr.role = 'owner' DESC, u.username`, modPath)
	if err != nil {
		return nil, fmt.Errorf("list collaborators of %s: %w", modPath, err)
	}
	defer rows.Close()
	out := []Collaborator{}
	for rows.Next() {
		var c Collaborator
		var since int64
		if err := rows.Scan(&c.Username, &c.Role, &since); err != nil {
			return nil, err
		}
		c.Since = time.Unix(since, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCollaborator gives username a role on the module, or changes it.
// Handing a module to someone else is: add them as owner, then remove
// yourself. The module path never changes, because code imports it.
func (r *Registry) SetCollaborator(ctx context.Context, u *accounts.User, modPath, username, roleName string, c accounts.Client) error {
	role, ok := parseRole(roleName)
	if !ok {
		return reject(http.StatusBadRequest, "invalid_role", "Choose owner or maintainer.")
	}
	moduleID, err := r.requireRole(ctx, u, modPath, RoleOwner)
	if err != nil {
		return err
	}
	username = accounts.NormalizeUsername(strings.TrimPrefix(strings.TrimSpace(username), "@"))
	var userID int64
	err = r.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return reject(http.StatusNotFound, "unknown_user", "There's no user @%s. Check the spelling; they need a Gopherdex account first.", username)
	}
	if err != nil {
		return fmt.Errorf("look up @%s: %w", username, err)
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO module_roles (module_id, user_id, role, created_by, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (module_id, user_id) DO UPDATE SET role = excluded.role`, moduleID, userID, role.String(), u.ID, r.now().Unix()); err != nil {
		return fmt.Errorf("set role on %s: %w", modPath, err)
	}
	if err := r.keepAnOwner(ctx, tx, moduleID, modPath); err != nil {
		return err
	}
	if err := r.audit(ctx, tx, u.ID, "module.role_set", fmt.Sprintf("%s @%s %s", modPath, username, role), c); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveCollaborator takes away username's role on the module.
func (r *Registry) RemoveCollaborator(ctx context.Context, u *accounts.User, modPath, username string, c accounts.Client) error {
	moduleID, err := r.requireRole(ctx, u, modPath, RoleOwner)
	if err != nil {
		return err
	}
	username = accounts.NormalizeUsername(strings.TrimPrefix(username, "@"))
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM module_roles WHERE module_id = ? AND user_id = (SELECT id FROM users WHERE username = ?)`, moduleID, username)
	if err != nil {
		return fmt.Errorf("remove @%s from %s: %w", username, modPath, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "not_a_collaborator", "@%s isn't a collaborator on %s.", username, modPath)
	}
	if err := r.keepAnOwner(ctx, tx, moduleID, modPath); err != nil {
		return err
	}
	if err := r.audit(ctx, tx, u.ID, "module.role_removed", fmt.Sprintf("%s @%s", modPath, username), c); err != nil {
		return err
	}
	return tx.Commit()
}

// keepAnOwner refuses changes that would leave a personal-namespace module
// with nobody who can manage it. Organization modules are always managed by
// the organization's owners.
func (r *Registry) keepAnOwner(ctx context.Context, tx *sql.Tx, moduleID int64, modPath string) error {
	var owners, isOrg int
	err := tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM module_roles WHERE module_id = ? AND role = 'owner'),
			(SELECT COUNT(*) FROM organizations o JOIN modules m ON m.namespace = o.name WHERE m.id = ?)`,
		moduleID, moduleID).Scan(&owners, &isOrg)
	if err != nil {
		return err
	}
	if owners == 0 && isOrg == 0 {
		return reject(http.StatusConflict, "last_owner", "%s needs at least one owner. Add another owner before removing or demoting this one.", modPath)
	}
	return nil
}

// Managed lists the modules u maintains, with their role, sorted by path.
func (r *Registry) Managed(ctx context.Context, u *accounts.User) ([]ModuleSummary, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT DISTINCT m.namespace FROM modules m
		LEFT JOIN module_roles mr ON mr.module_id = m.id AND mr.user_id = ?
		LEFT JOIN organizations o ON o.name = m.namespace
		LEFT JOIN org_members om ON om.org_id = o.id AND om.user_id = ?
		WHERE mr.user_id IS NOT NULL OR om.user_id IS NOT NULL`, u.ID, u.ID)
	if err != nil {
		return nil, fmt.Errorf("list managed modules: %w", err)
	}
	var namespaces []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			rows.Close()
			return nil, err
		}
		namespaces = append(namespaces, ns)
	}
	rows.Close()
	var out []ModuleSummary
	for _, ns := range namespaces {
		mods, err := r.NamespaceModules(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, m := range mods {
			if role, err := r.Role(ctx, u, m.Path); err == nil && role >= RoleMaintainer {
				out = append(out, m)
			}
		}
	}
	return out, nil
}
