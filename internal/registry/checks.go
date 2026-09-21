package registry

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	xmodule "golang.org/x/mod/module"

	"github.com/parthiban-sivakumar/gopherdex/internal/scan"
)

// runChecks scans an upload before it's stored. Blocking findings refuse
// the upload; the rest are returned to be recorded and reviewed.
func (r *Registry) runChecks(ctx context.Context, u Upload, namespace string) ([]scan.Finding, error) {
	findings, err := scan.Zip(u.ZipFile, u.Module+"@"+u.Version+"/")
	if err != nil {
		return nil, reject(http.StatusUnprocessableEntity, "invalid_zip", "The module zip couldn't be checked: %v", err)
	}
	// Name checks apply when a module is first published; after that its
	// name is settled.
	var exists bool
	if err := r.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM modules WHERE path = ?)`, u.Module).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		popular, err := r.popularNames(ctx, namespace, 500)
		if err != nil {
			return nil, err
		}
		prefix, _, _ := xmodule.SplitPathVersion(u.Module)
		findings = append(findings, scan.Typosquat(namespace+"/"+path.Base(prefix), popular)...)
	}
	if scan.Blocked(findings) {
		var why []string
		for _, f := range findings {
			if f.Severity == scan.Block {
				why = append(why, f.String())
			}
		}
		return nil, reject(http.StatusUnprocessableEntity, "blocked_by_checks",
			"%s@%s can't be published: %s Remove the file, tag a new release, and publish again.", u.Module, u.Version, strings.Join(why, " "))
	}
	return findings, nil
}

// popularNames lists owner/name of the most downloaded modules outside
// namespace, for typosquatting checks.
//
// Ranking every module by downloads takes a pass over the whole registry,
// so on a large registry the ranking is cached for popularTTL and filtered
// per namespace here. It fetches twice the limit so one busy namespace
// can't crowd out the rest.
func (r *Registry) popularNames(ctx context.Context, namespace string, limit int) ([]string, error) {
	r.popularMu.Lock()
	defer r.popularMu.Unlock()
	// A short ranking means a small registry: the query is cheap there, and
	// a module published a moment ago must count, so don't reuse it.
	// Freshness is measured in real time, not r.now(): imports and tests
	// set the registry's clock to when each version was published.
	if len(r.popular) < 2*limit || time.Since(r.popularAt) >= popularTTL {
		rows, err := r.DB.QueryContext(ctx, `SELECT m.namespace, m.path FROM modules m LEFT JOIN downloads d ON d.module_id = m.id
			WHERE m.quarantined_at IS NULL
			GROUP BY m.id ORDER BY COALESCE(SUM(d.count), 0) DESC, m.id LIMIT ?`, 2*limit)
		if err != nil {
			return nil, fmt.Errorf("popular modules: %w", err)
		}
		defer rows.Close()
		var ranked []popularModule
		for rows.Next() {
			var pm popularModule
			if err := rows.Scan(&pm.namespace, &pm.path); err != nil {
				return nil, err
			}
			ranked = append(ranked, pm)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		r.popular, r.popularAt = ranked, time.Now()
	}
	var out []string
	for _, pm := range r.popular {
		if len(out) == limit {
			break
		}
		if pm.namespace != namespace {
			prefix, _, _ := xmodule.SplitPathVersion(strings.TrimPrefix(pm.path, r.ModuleHost+"/"))
			out = append(out, prefix)
		}
	}
	return out, nil
}

// popularTTL is how long the download ranking for name checks is reused.
// New modules only enter it by being downloaded, which takes longer.
const popularTTL = 10 * time.Minute

type popularModule struct{ namespace, path string }

// recordFindings stores a version's warnings and opens a report so an
// administrator looks at them.
func (r *Registry) recordFindings(ctx context.Context, moduleID, versionID int64, modPath, version string, findings []scan.Finding) error {
	if len(findings) == 0 {
		return nil
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	category := "malware"
	var lines []string
	for _, f := range findings {
		if f.Rule == "typosquatting" || f.Rule == "impersonation" {
			category = "typosquatting"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO publish_findings (version_id, rule, severity, file, line, message) VALUES (?, ?, ?, ?, ?, ?)`,
			versionID, f.Rule, string(f.Severity), f.File, f.Line, f.Message); err != nil {
			return fmt.Errorf("record findings: %w", err)
		}
		lines = append(lines, "- "+f.String())
	}
	details := fmt.Sprintf("Automated publish checks flagged %s@%s:\n%s", modPath, version, strings.Join(lines, "\n"))
	// reporter_id is NULL: the registry itself reported it.
	if _, err := tx.ExecContext(ctx, `INSERT INTO reports (module_id, reporter_id, category, details, created_at) VALUES (?, NULL, ?, ?, ?)`,
		moduleID, category, details, r.now().Unix()); err != nil {
		return fmt.Errorf("record findings: %w", err)
	}
	return tx.Commit()
}

// VersionFindings returns the stored warnings of a module's versions, by
// version.
func (r *Registry) VersionFindings(ctx context.Context, modPath string) (map[string][]scan.Finding, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT v.version, f.rule, f.severity, f.file, f.line, f.message
		FROM publish_findings f JOIN versions v ON v.id = f.version_id JOIN modules m ON m.id = v.module_id
		WHERE m.path = ? ORDER BY f.id`, modPath)
	if err != nil {
		return nil, fmt.Errorf("findings of %s: %w", modPath, err)
	}
	defer rows.Close()
	out := map[string][]scan.Finding{}
	for rows.Next() {
		var v string
		var f scan.Finding
		var sev string
		if err := rows.Scan(&v, &f.Rule, &sev, &f.File, &f.Line, &f.Message); err != nil {
			return nil, err
		}
		f.Severity = scan.Severity(sev)
		out[v] = append(out[v], f)
	}
	return out, rows.Err()
}
