package registry

import (
	"context"
	"fmt"
	"time"
)

// Release is one published version, for feeds.
type Release struct {
	Path        string
	Version     string
	Synopsis    string
	PublishedAt time.Time
	PublishedBy string // username, or "" if the account is gone
	Trusted     bool   // uploaded by a trusted publisher
}

// Releases lists installable releases, newest first: all of them, a
// namespace's (namespace != ""), or one module's (modPath != "").
// Yanked versions and quarantined modules are left out.
func (r *Registry) Releases(ctx context.Context, namespace, modPath string, limit int) ([]Release, error) {
	q := `SELECT m.path, v.version, v.synopsis, v.published_at, COALESCE(u.username, ''), v.provenance != ''
		FROM versions v JOIN modules m ON m.id = v.module_id LEFT JOIN users u ON u.id = v.published_by
		WHERE m.quarantined_at IS NULL AND v.yanked_at IS NULL`
	var args []any
	if namespace != "" {
		q += ` AND v.namespace = ?` // indexed with published_at, newest first
		args = append(args, namespace)
	}
	if modPath != "" {
		q += ` AND m.path = ?`
		args = append(args, modPath)
	}
	q += ` ORDER BY v.published_at DESC, v.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		var rel Release
		var published int64
		if err := rows.Scan(&rel.Path, &rel.Version, &rel.Synopsis, &published, &rel.PublishedBy, &rel.Trusted); err != nil {
			return nil, fmt.Errorf("list releases: %w", err)
		}
		rel.PublishedAt = time.Unix(published, 0).UTC()
		out = append(out, rel)
	}
	return out, rows.Err()
}
