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
)

// Org is an organization: a shared namespace, e.g. gopherdex.dev/acme/…,
// whose members publish modules together.
type Org struct {
	Name         string
	DisplayName  string
	MemberAccess string // MemberAccessMaintainer or MemberAccessNone
	CreatedAt    time.Time
}

// OrgMember is a user in an organization. Owners manage members and teams
// and act as owners of every module in the namespace; members publish and
// yank every module, or, when the organization's member access is "none",
// the modules their teams cover.
type OrgMember struct {
	Username string
	Role     string // "owner" or "member"
	Since    time.Time
}

// Membership is one organization a user belongs to.
type Membership struct {
	Org  string
	Role string
}

// CreateOrg creates an organization and makes u its first owner.
func (r *Registry) CreateOrg(ctx context.Context, u *accounts.User, name, displayName string, c accounts.Client) error {
	name = accounts.NormalizeUsername(name)
	if err := accounts.ValidateNamespace(name); err != nil {
		var fe *accounts.FieldError
		if errors.As(err, &fe) {
			return reject(http.StatusBadRequest, "invalid_name", "%s", strings.Replace(fe.Message, "username", "name", 1))
		}
		return err
	}
	displayName = strings.TrimSpace(displayName)
	if len(displayName) > 100 {
		return reject(http.StatusBadRequest, "invalid_display_name", "Keep the display name under 100 characters.")
	}
	if !u.EmailVerified {
		return reject(http.StatusForbidden, "email_not_verified", "Verify your email address before creating an organization.")
	}
	now := r.now().Unix()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO namespaces (name, kind, created_at) VALUES (?, 'org', ?) ON CONFLICT DO NOTHING`, name, now)
	if err != nil {
		return fmt.Errorf("create organization %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusConflict, "name_taken", "The name %s is taken by a user or organization.", name)
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO organizations (name, display_name, created_by, created_at) VALUES (?, ?, ?, ?)`, name, displayName, u.ID, now)
	if err != nil {
		return fmt.Errorf("create organization %s: %w", name, err)
	}
	orgID, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `INSERT INTO org_members (org_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)`, orgID, u.ID, now); err != nil {
		return fmt.Errorf("create organization %s: %w", name, err)
	}
	if err := r.audit(ctx, tx, u.ID, "org.created", name, c); err != nil {
		return err
	}
	return tx.Commit()
}

// OrgByName returns an organization, or ok=false if name is not one.
func (r *Registry) OrgByName(ctx context.Context, name string) (*Org, bool, error) {
	var o Org
	var created int64
	err := r.DB.QueryRowContext(ctx, `SELECT name, display_name, member_access, created_at FROM organizations WHERE name = ?`, name).Scan(&o.Name, &o.DisplayName, &o.MemberAccess, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("look up organization %s: %w", name, err)
	}
	o.CreatedAt = time.Unix(created, 0).UTC()
	return &o, true, nil
}

// OrgMembers lists an organization's members, owners first.
func (r *Registry) OrgMembers(ctx context.Context, org string) ([]OrgMember, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT u.username, om.role, om.created_at FROM org_members om
		JOIN organizations o ON o.id = om.org_id JOIN users u ON u.id = om.user_id
		WHERE o.name = ? ORDER BY om.role = 'owner' DESC, u.username`, org)
	if err != nil {
		return nil, fmt.Errorf("list members of %s: %w", org, err)
	}
	defer rows.Close()
	out := []OrgMember{}
	for rows.Next() {
		var m OrgMember
		var since int64
		if err := rows.Scan(&m.Username, &m.Role, &since); err != nil {
			return nil, err
		}
		m.Since = time.Unix(since, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// Memberships lists the organizations a user belongs to.
func (r *Registry) Memberships(ctx context.Context, userID int64) ([]Membership, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT o.name, om.role FROM org_members om JOIN organizations o ON o.id = om.org_id
		WHERE om.user_id = ? ORDER BY o.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.Org, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// orgMember returns u's role in the organization named ns, or "".
func (r *Registry) orgMember(ctx context.Context, ns string, userID int64) (string, error) {
	var role string
	err := r.DB.QueryRowContext(ctx, `SELECT om.role FROM org_members om JOIN organizations o ON o.id = om.org_id
		WHERE o.name = ? AND om.user_id = ?`, ns, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up membership of %s: %w", ns, err)
	}
	return role, nil
}

// IsOrgOwner reports whether u owns the organization.
func (r *Registry) IsOrgOwner(ctx context.Context, u *accounts.User, org string) (bool, error) {
	if u == nil {
		return false, nil
	}
	role, err := r.orgMember(ctx, org, u.ID)
	return role == "owner", err
}

// SetOrgMember adds username to the organization or changes their role.
// Only organization owners may do this.
func (r *Registry) SetOrgMember(ctx context.Context, u *accounts.User, org, username, role string, c accounts.Client) error {
	if role != "owner" && role != "member" {
		return reject(http.StatusBadRequest, "invalid_role", "Choose owner or member.")
	}
	orgID, err := r.requireOrgOwner(ctx, u, org)
	if err != nil {
		return err
	}
	username = accounts.NormalizeUsername(strings.TrimPrefix(strings.TrimSpace(username), "@"))
	var userID int64
	err = r.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return reject(http.StatusNotFound, "unknown_user", "There's no user @%s. They need a Gopherdex account first.", username)
	}
	if err != nil {
		return err
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO org_members (org_id, user_id, role, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (org_id, user_id) DO UPDATE SET role = excluded.role`, orgID, userID, role, r.now().Unix()); err != nil {
		return fmt.Errorf("add @%s to %s: %w", username, org, err)
	}
	if err := keepAnOrgOwner(ctx, tx, orgID, org); err != nil {
		return err
	}
	if err := r.audit(ctx, tx, u.ID, "org.member_set", fmt.Sprintf("%s @%s %s", org, username, role), c); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveOrgMember removes username from the organization.
func (r *Registry) RemoveOrgMember(ctx context.Context, u *accounts.User, org, username string, c accounts.Client) error {
	orgID, err := r.requireOrgOwner(ctx, u, org)
	if err != nil {
		return err
	}
	username = accounts.NormalizeUsername(strings.TrimPrefix(username, "@"))
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM org_members WHERE org_id = ? AND user_id = (SELECT id FROM users WHERE username = ?)`, orgID, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "not_a_member", "@%s isn't a member of %s.", username, org)
	}
	if err := keepAnOrgOwner(ctx, tx, orgID, org); err != nil {
		return err
	}
	if err := r.audit(ctx, tx, u.ID, "org.member_removed", fmt.Sprintf("%s @%s", org, username), c); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Registry) requireOrgOwner(ctx context.Context, u *accounts.User, org string) (int64, error) {
	var orgID int64
	var role sql.NullString
	err := r.DB.QueryRowContext(ctx, `SELECT o.id, (SELECT role FROM org_members WHERE org_id = o.id AND user_id = ?)
		FROM organizations o WHERE o.name = ?`, u.ID, org).Scan(&orgID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, reject(http.StatusNotFound, "unknown_org", "There's no organization %s.", org)
	}
	if err != nil {
		return 0, err
	}
	if role.String != "owner" {
		return 0, reject(http.StatusForbidden, "forbidden", "Only owners of %s can manage its members and teams.", org)
	}
	return orgID, nil
}

func keepAnOrgOwner(ctx context.Context, tx *sql.Tx, orgID int64, org string) error {
	var owners int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM org_members WHERE org_id = ? AND role = 'owner'`, orgID).Scan(&owners); err != nil {
		return err
	}
	if owners == 0 {
		return reject(http.StatusConflict, "last_owner", "%s needs at least one owner. Make someone else an owner first.", org)
	}
	return nil
}
