package registry

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/parthiban-sivakumar/gopherdex/internal/gomod"
)

// recordRequires stores the requirements from a version's go.mod.
func recordRequires(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, versionID int64, goMod []byte) error {
	f, err := gomod.Parse(goMod)
	if err == nil {
		for _, req := range f.Require {
			if _, err := db.ExecContext(ctx, `INSERT INTO version_requires (version_id, path, version, indirect) VALUES (?, ?, ?, ?)
				ON CONFLICT DO NOTHING`, versionID, req.Path, req.Version, req.Indirect); err != nil {
				return fmt.Errorf("record requirements: %w", err)
			}
		}
	}
	// A go.mod the parser can't read still counts as indexed: it has
	// nothing to record.
	if _, err := db.ExecContext(ctx, `UPDATE versions SET requires_indexed = 1 WHERE id = ?`, versionID); err != nil {
		return fmt.Errorf("record requirements: %w", err)
	}
	return nil
}

// BackfillRequires records the requirements of versions published before
// the dependency graph existed.
func (r *Registry) BackfillRequires(ctx context.Context) error {
	rows, err := r.DB.QueryContext(ctx, `SELECT id, go_mod FROM versions WHERE requires_indexed = 0`)
	if err != nil {
		return fmt.Errorf("backfill requirements: %w", err)
	}
	type pending struct {
		id    int64
		goMod []byte
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.goMod); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	for _, p := range todo {
		if err := recordRequires(ctx, r.DB, p.id, p.goMod); err != nil {
			return err
		}
	}
	if len(todo) > 0 {
		r.log().Info("recorded requirements of earlier versions", "versions", len(todo))
	}
	return nil
}

// Dependent is a hosted module whose latest release requires another.
type Dependent struct {
	Path     string // the dependent module
	Version  string // its latest release
	Requires string // the version of the dependency it requires
	Indirect bool
}

// latestVersionIDs picks each visible module's latest installable release,
// like the listings do.
const latestVersionIDs = `SELECT v.id FROM modules m JOIN versions v ON v.module_id = m.id
	WHERE m.quarantined_at IS NULL
	  AND v.id = (SELECT id FROM versions WHERE module_id = m.id AND yanked_at IS NULL ORDER BY published_at DESC, id DESC LIMIT 1)`

// Dependents lists the hosted modules whose latest release requires
// modPath, directly first.
func (r *Registry) Dependents(ctx context.Context, modPath string, limit int) ([]Dependent, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT m.path, v.version, vr.version, vr.indirect
		FROM version_requires vr JOIN versions v ON v.id = vr.version_id JOIN modules m ON m.id = v.module_id
		WHERE vr.path = ? AND vr.version_id IN (`+latestVersionIDs+`)
		ORDER BY vr.indirect, m.path LIMIT ?`, modPath, limit)
	if err != nil {
		return nil, fmt.Errorf("dependents of %s: %w", modPath, err)
	}
	defer rows.Close()
	var out []Dependent
	for rows.Next() {
		var d Dependent
		if err := rows.Scan(&d.Path, &d.Version, &d.Requires, &d.Indirect); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DependentCounts counts the hosted modules whose latest release requires
// modPath: directly, and in total.
func (r *Registry) DependentCounts(ctx context.Context, modPath string) (direct, total int, err error) {
	err = r.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(vr.indirect = 0), 0), COUNT(*) FROM version_requires vr
		WHERE vr.path = ? AND vr.version_id IN (`+latestVersionIDs+`)`, modPath).Scan(&direct, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("count dependents of %s: %w", modPath, err)
	}
	return direct, total, nil
}
