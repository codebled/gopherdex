package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/codebled/gopherdex/internal/accounts"
)

// Being made a member of an organization, or an owner or maintainer of a
// module, is an invitation until the person accepts it. Until then it
// gives no access and doesn't count as an owner, so nobody can be put in
// charge of something without agreeing to it. (Security review: an owner
// could make a well-known developer the owner of a malicious module, then
// leave, leaving them as its sole owner, unable even to delete their
// account.) Anyone can also leave an organization or give up a module role,
// as long as it keeps an owner.

// Invitation kinds.
const (
	InviteOrg    = "organization"
	InviteModule = "module"
)

// Invitation is a pending role for a user.
type Invitation struct {
	Kind      string // InviteOrg or InviteModule
	Name      string // the organization or module path
	Role      string
	InvitedBy string
	Since     time.Time
}

// Invitations lists u's pending invitations, oldest first.
func (r *Registry) Invitations(ctx context.Context, u *accounts.User) ([]Invitation, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT 'organization', o.name, om.role, COALESCE(i.username, ''), om.created_at
			FROM org_members om JOIN organizations o ON o.id = om.org_id LEFT JOIN users i ON i.id = om.invited_by
			WHERE om.user_id = ? AND om.accepted_at IS NULL
		UNION ALL
		SELECT 'module', m.path, mr.role, COALESCE(i.username, ''), mr.created_at
			FROM module_roles mr JOIN modules m ON m.id = mr.module_id LEFT JOIN users i ON i.id = mr.created_by
			WHERE mr.user_id = ? AND mr.accepted_at IS NULL
		ORDER BY 5, 2`, u.ID, u.ID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	defer rows.Close()
	out := []Invitation{}
	for rows.Next() {
		var in Invitation
		var since int64
		if err := rows.Scan(&in.Kind, &in.Name, &in.Role, &in.InvitedBy, &since); err != nil {
			return nil, err
		}
		in.Since = time.Unix(since, 0).UTC()
		out = append(out, in)
	}
	return out, rows.Err()
}

// InvitationPending reports whether username has an unanswered invitation
// to the organization or module name, e.g. to word a notification.
func (r *Registry) InvitationPending(ctx context.Context, kind, name, username string) (bool, error) {
	q := `SELECT EXISTS (SELECT 1 FROM org_members om JOIN organizations o ON o.id = om.org_id JOIN users u ON u.id = om.user_id
		WHERE o.name = ? AND u.username = ? AND om.accepted_at IS NULL)`
	if kind == InviteModule {
		q = `SELECT EXISTS (SELECT 1 FROM module_roles mr JOIN modules m ON m.id = mr.module_id JOIN users u ON u.id = mr.user_id
			WHERE m.path = ? AND u.username = ? AND mr.accepted_at IS NULL)`
	}
	var pending bool
	err := r.DB.QueryRowContext(ctx, q, name, accounts.NormalizeUsername(username)).Scan(&pending)
	return pending, err
}

// AcceptInvitation turns u's invitation into a role.
func (r *Registry) AcceptInvitation(ctx context.Context, u *accounts.User, kind, name string, c accounts.Client) error {
	q := `UPDATE org_members SET accepted_at = ? WHERE user_id = ? AND accepted_at IS NULL
		AND org_id = (SELECT id FROM organizations WHERE name = ?)`
	if kind == InviteModule {
		q = `UPDATE module_roles SET accepted_at = ? WHERE user_id = ? AND accepted_at IS NULL
			AND module_id = (SELECT id FROM modules WHERE path = ?)`
	}
	res, err := r.DB.ExecContext(ctx, q, r.now().Unix(), u.ID, name)
	if err != nil {
		return fmt.Errorf("accept invitation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "no_invitation", "There's no pending invitation to %s.", name)
	}
	return r.audit(ctx, r.DB, u.ID, "invitation.accepted", kind+" "+name, c)
}

// DeclineInvitation drops u's invitation.
func (r *Registry) DeclineInvitation(ctx context.Context, u *accounts.User, kind, name string, c accounts.Client) error {
	q := `DELETE FROM org_members WHERE user_id = ? AND accepted_at IS NULL
		AND org_id = (SELECT id FROM organizations WHERE name = ?)`
	if kind == InviteModule {
		q = `DELETE FROM module_roles WHERE user_id = ? AND accepted_at IS NULL
			AND module_id = (SELECT id FROM modules WHERE path = ?)`
	}
	res, err := r.DB.ExecContext(ctx, q, u.ID, name)
	if err != nil {
		return fmt.Errorf("decline invitation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "no_invitation", "There's no pending invitation to %s.", name)
	}
	return r.audit(ctx, r.DB, u.ID, "invitation.declined", kind+" "+name, c)
}

// LeaveOrg takes u out of an organization. The last owner can't leave:
// someone else must become an owner first.
func (r *Registry) LeaveOrg(ctx context.Context, u *accounts.User, org string, c accounts.Client) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var orgID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM organizations WHERE name = ?`, org).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return reject(http.StatusNotFound, "unknown_org", "There's no organization %s.", org)
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM org_members WHERE org_id = ? AND user_id = ?`, orgID, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "not_a_member", "You aren't a member of %s.", org)
	}
	if err := keepAnOrgOwner(ctx, tx, orgID, org); err != nil {
		return err
	}
	if err := r.audit(ctx, tx, u.ID, "org.left", org, c); err != nil {
		return err
	}
	return tx.Commit()
}

// LeaveModule gives up u's own role on a module, as long as it keeps an
// owner.
func (r *Registry) LeaveModule(ctx context.Context, u *accounts.User, modPath string, c accounts.Client) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var moduleID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM modules WHERE path = ?`, modPath).Scan(&moduleID)
	if errors.Is(err, sql.ErrNoRows) {
		return reject(http.StatusNotFound, "unknown_module", "There's no module %s.", modPath)
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM module_roles WHERE module_id = ? AND user_id = ?`, moduleID, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "not_a_collaborator", "You don't have a role on %s.", modPath)
	}
	if err := r.keepAnOwner(ctx, tx, moduleID, modPath); err != nil {
		return err
	}
	if err := r.audit(ctx, tx, u.ID, "module.left", modPath, c); err != nil {
		return err
	}
	return tx.Commit()
}
