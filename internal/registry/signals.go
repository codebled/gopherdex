package registry

import (
	"context"
	"fmt"
	"strings"
)

// Signal is what search ranking knows about a hosted module beyond text.
type Signal struct {
	UsedBy     int  // modules whose latest release requires it
	Verified   bool // its latest release came from a trusted publisher
	Vulnerable bool // an active advisory affects its latest release
}

// Signals returns ranking signals for search hits, in a few queries for
// the whole page rather than several per module.
func (r *Registry) Signals(ctx context.Context, hits []SearchHit) (map[string]Signal, error) {
	out := make(map[string]Signal, len(hits))
	if len(hits) == 0 {
		return out, nil
	}
	paths := make([]any, len(hits))
	marks := make([]string, len(hits))
	latest := map[string]string{}
	for i, h := range hits {
		paths[i], marks[i] = h.Path, "?"
		out[h.Path] = Signal{}
		latest[h.Path] = h.Version
	}
	in := "(" + strings.Join(marks, ",") + ")"

	rows, err := r.DB.QueryContext(ctx, `SELECT vr.path, COUNT(*)
		FROM version_requires vr JOIN versions v ON v.id = vr.version_id JOIN modules m ON m.id = v.module_id
		WHERE vr.path IN `+in+` AND `+isLatest+` GROUP BY vr.path`, paths...)
	if err != nil {
		return nil, fmt.Errorf("search signals: %w", err)
	}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			rows.Close()
			return nil, err
		}
		s := out[p]
		s.UsedBy = n
		out[p] = s
	}
	rows.Close()

	rows, err = r.DB.QueryContext(ctx, `SELECT m.path, v.provenance != '' FROM modules m JOIN versions v ON v.module_id = m.id
		WHERE m.path IN `+in+` AND `+isLatest, paths...)
	if err != nil {
		return nil, fmt.Errorf("search signals: %w", err)
	}
	for rows.Next() {
		var p string
		var verified bool
		if err := rows.Scan(&p, &verified); err != nil {
			rows.Close()
			return nil, err
		}
		s := out[p]
		s.Verified = verified
		out[p] = s
	}
	rows.Close()

	advisories, err := r.advisories(ctx, ` AND m.path IN `+in, paths...)
	if err != nil {
		return nil, err
	}
	for _, a := range advisories {
		if a.Affects(latest[a.ModulePath]) {
			s := out[a.ModulePath]
			s.Vulnerable = true
			out[a.ModulePath] = s
		}
	}
	return out, nil
}
