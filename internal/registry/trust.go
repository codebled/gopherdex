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

// ErrQuarantined reports a module hidden while administrators review it. It
// matches module.ErrNotFound, so the go command sees a plain "not found".
var ErrQuarantined = fmt.Errorf("under review by the registry's administrators: %w", module.ErrNotFound)

// TokenAllows reports whether an API token may act on modPath: a user-wide
// token covers everything its user maintains; a module token covers one
// module.
func TokenAllows(tok *accounts.Token, u *accounts.User, modPath string) error {
	if tok == nil || u == nil {
		return reject(http.StatusUnauthorized, "unauthenticated", "Publishing needs an API token.")
	}
	if tok.Scope == accounts.UserScope(u) {
		return nil
	}
	if m, ok := strings.CutPrefix(tok.Scope, accounts.ScopeModulePrefix); ok {
		if m == modPath {
			return nil
		}
		return reject(http.StatusForbidden, "token_scope", "This API token only works for %s, not %s. Use a token for all your modules, or create one for this module.", m, modPath)
	}
	return reject(http.StatusForbidden, "forbidden_token", "This API token can't act on modules.")
}

func (r *Registry) quarantined(ctx context.Context, modPath string) (bool, error) {
	var q sql.NullInt64
	err := r.DB.QueryRowContext(ctx, `SELECT quarantined_at FROM modules WHERE path = ?`, modPath).Scan(&q)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return q.Valid, err
}

// Maintainers lists everyone who should hear about changes to a module:
// its owners and maintainers, the members of teams with access, and for
// organization modules, the organization's owners (and its members, unless
// member access is "none").
func (r *Registry) Maintainers(ctx context.Context, modPath string) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT u.username FROM module_roles mr JOIN modules m ON m.id = mr.module_id JOIN users u ON u.id = mr.user_id WHERE m.path = ?
		UNION
		SELECT u.username FROM modules m JOIN organizations o ON o.name = m.namespace JOIN org_members om ON om.org_id = o.id
			JOIN users u ON u.id = om.user_id WHERE m.path = ? AND (om.role = 'owner' OR o.member_access = 'maintainer')
		UNION
		SELECT u.username FROM modules m JOIN team_modules tm ON tm.module_id = m.id JOIN team_members tu ON tu.team_id = tm.team_id
			JOIN users u ON u.id = tu.user_id WHERE m.path = ?
		ORDER BY 1`, modPath, modPath, modPath)
	if err != nil {
		return nil, fmt.Errorf("maintainers of %s: %w", modPath, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ---- Reports ----

// Report categories.
var ReportCategories = []string{"malware", "typosquatting", "spam", "license", "other"}

// Report is a user's report about a module.
type Report struct {
	ID          int64
	Module      string
	Reporter    string
	Category    string
	Details     string
	CreatedAt   time.Time
	Quarantined bool
}

// ReportModule records a report for administrators to review.
func (r *Registry) ReportModule(ctx context.Context, u *accounts.User, modPath, category, details string, c accounts.Client) error {
	details = strings.TrimSpace(details)
	valid := false
	for _, cat := range ReportCategories {
		valid = valid || cat == category
	}
	switch {
	case !valid:
		return reject(http.StatusBadRequest, "invalid_category", "Choose what kind of problem this is.")
	case len(details) < 10:
		return reject(http.StatusBadRequest, "missing_details", "Describe the problem in a sentence or two, so reviewers know where to look.")
	case len(details) > 4000:
		return reject(http.StatusBadRequest, "details_too_long", "Keep the description under 4,000 characters.")
	}
	var moduleID int64
	err := r.DB.QueryRowContext(ctx, `SELECT id FROM modules WHERE path = ?`, modPath).Scan(&moduleID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", modPath, module.ErrNotFound)
	}
	if err != nil {
		return err
	}
	if _, err := r.DB.ExecContext(ctx, `INSERT INTO reports (module_id, reporter_id, category, details, created_at) VALUES (?, ?, ?, ?, ?)`,
		moduleID, u.ID, category, details, r.now().Unix()); err != nil {
		return fmt.Errorf("save report: %w", err)
	}
	return r.audit(ctx, r.DB, u.ID, "module.reported", modPath+" "+category, c)
}

// OpenReports lists reports waiting for review, oldest first.
func (r *Registry) OpenReports(ctx context.Context) ([]Report, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT rp.id, m.path, COALESCE(u.username, ''), rp.category, rp.details, rp.created_at, m.quarantined_at IS NOT NULL
		FROM reports rp JOIN modules m ON m.id = rp.module_id LEFT JOIN users u ON u.id = rp.reporter_id
		WHERE rp.status = 'open' ORDER BY rp.created_at, rp.id`)
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		var rp Report
		var created int64
		if err := rows.Scan(&rp.ID, &rp.Module, &rp.Reporter, &rp.Category, &rp.Details, &created, &rp.Quarantined); err != nil {
			return nil, err
		}
		rp.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, rp)
	}
	return out, rows.Err()
}

// DismissReport closes a report without action.
func (r *Registry) DismissReport(ctx context.Context, admin *accounts.User, id int64, note string, c accounts.Client) error {
	res, err := r.DB.ExecContext(ctx, `UPDATE reports SET status = 'dismissed', resolved_at = ?, resolved_by = ?, resolution = ? WHERE id = ? AND status = 'open'`,
		r.now().Unix(), admin.ID, strings.TrimSpace(note), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return reject(http.StatusNotFound, "no_report", "That report is already closed.")
	}
	return r.audit(ctx, r.DB, admin.ID, "report.dismissed", fmt.Sprintf("report=%d %s", id, note), c)
}

// QuarantinedModule is a module hidden for review.
type QuarantinedModule struct {
	Path   string
	Reason string
	Since  time.Time
}

// Quarantine hides a module from pages, search and the proxy, and closes
// its open reports. The files are kept, so releasing it restores
// everything.
func (r *Registry) Quarantine(ctx context.Context, admin *accounts.User, modPath, reason string, c accounts.Client) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return reject(http.StatusBadRequest, "missing_reason", "Give a reason; the module's owners are told it.")
	}
	now := r.now().Unix()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM modules WHERE path = ?`, modPath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", modPath, module.ErrNotFound)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE modules SET quarantined_at = ?, quarantine_reason = ? WHERE id = ?`, now, reason, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE reports SET status = 'actioned', resolved_at = ?, resolved_by = ?, resolution = ? WHERE module_id = ? AND status = 'open'`,
		now, admin.ID, "quarantined: "+reason, id); err != nil {
		return err
	}
	if err := r.audit(ctx, tx, admin.ID, "module.quarantined", modPath+" "+reason, c); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return r.reindex(ctx, id)
}

// Release ends a quarantine.
func (r *Registry) Release(ctx context.Context, admin *accounts.User, modPath string, c accounts.Client) error {
	var id int64
	if err := r.DB.QueryRowContext(ctx, `SELECT id FROM modules WHERE path = ?`, modPath).Scan(&id); err != nil {
		return fmt.Errorf("%s: %w", modPath, module.ErrNotFound)
	}
	if _, err := r.DB.ExecContext(ctx, `UPDATE modules SET quarantined_at = NULL, quarantine_reason = '' WHERE id = ?`, id); err != nil {
		return err
	}
	if err := r.audit(ctx, r.DB, admin.ID, "module.released", modPath, c); err != nil {
		return err
	}
	return r.reindex(ctx, id)
}

// QuarantinedModules lists modules under review.
func (r *Registry) QuarantinedModules(ctx context.Context) ([]QuarantinedModule, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT path, quarantine_reason, quarantined_at FROM modules WHERE quarantined_at IS NOT NULL ORDER BY quarantined_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QuarantinedModule{}
	for rows.Next() {
		var q QuarantinedModule
		var since int64
		if err := rows.Scan(&q.Path, &q.Reason, &since); err != nil {
			return nil, err
		}
		q.Since = time.Unix(since, 0).UTC()
		out = append(out, q)
	}
	return out, rows.Err()
}
