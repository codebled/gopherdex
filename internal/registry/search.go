package registry

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	xmodule "golang.org/x/mod/module"

	"github.com/parthiban-sivakumar/gopherdex/internal/godoc"
	"github.com/parthiban-sivakumar/gopherdex/internal/gomod"
	"github.com/parthiban-sivakumar/gopherdex/internal/license"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

const maxIndexedReadme = 20 << 10

// versionMeta is what search needs from a version's files.
type versionMeta struct {
	Synopsis  string
	Readme    string // plain text, trimmed
	License   string // SPDX IDs, comma-separated
	GoVersion string
}

// metaFromFS extracts search details from a version's source tree. It never
// fails a publish: anything unreadable is simply left blank.
func metaFromFS(ctx context.Context, fsys fs.FS, modPath string) versionMeta {
	var m versionMeta
	if pkgs, err := godoc.Extract(ctx, fsys, modPath); err == nil && len(pkgs) > 0 {
		m.Synopsis = pkgs[0].Synopsis
		for _, p := range pkgs {
			if p.ImportPath == modPath && p.Synopsis != "" {
				m.Synopsis = p.Synopsis
			}
		}
	}
	root, _ := fs.ReadDir(fsys, ".")
	find := func(names []string) string {
		for _, want := range names {
			for _, e := range root {
				if !e.IsDir() && strings.EqualFold(e.Name(), want) {
					return e.Name()
				}
			}
		}
		return ""
	}
	read := func(name string, limit int64) []byte {
		f, err := fsys.Open(name)
		if err != nil {
			return nil
		}
		defer f.Close()
		b, _ := io.ReadAll(io.LimitReader(f, limit))
		return b
	}
	if name := find([]string{"README.md", "README.markdown", "README", "README.txt"}); name != "" {
		m.Readme = strings.TrimSpace(string(read(name, maxIndexedReadme)))
	}
	if name := find(license.FileNames); name != "" {
		m.License = strings.Join(license.Detect(read(name, 1<<20)), ",")
	}
	if f, err := gomod.Parse(read("go.mod", 1<<20)); err == nil {
		m.GoVersion = f.Go
	}
	return m
}

func zipMeta(ctx context.Context, zipFile string, mv xmodule.Version) versionMeta {
	zr, err := zip.OpenReader(zipFile)
	if err != nil {
		return versionMeta{}
	}
	defer zr.Close()
	fsys, err := fs.Sub(zr, mv.Path+"@"+mv.Version)
	if err != nil {
		return versionMeta{}
	}
	return metaFromFS(ctx, fsys, mv.Path)
}

// GoNum turns a go directive into a comparable number: "1.22.3" → 1022.
func GoNum(v string) int {
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	ma, err1 := strconv.Atoi(major)
	mi, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil {
		return 0
	}
	return ma*1000 + mi
}

// reindex refreshes a module's search entry from its latest installable
// version. Call it after anything that changes what search should show.
func (r *Registry) reindex(ctx context.Context, moduleID int64) error {
	var modPath, namespace, deprecation string
	var created int64
	var quarantined sql.NullInt64
	if err := r.DB.QueryRowContext(ctx, `SELECT path, namespace, created_at, deprecation, quarantined_at FROM modules WHERE id = ?`, moduleID).
		Scan(&modPath, &namespace, &created, &deprecation, &quarantined); err != nil {
		return fmt.Errorf("reindex module %d: %w", moduleID, err)
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT version, published_at, synopsis, readme_text, license, go_version
		FROM versions WHERE module_id = ? AND yanked_at IS NULL`, moduleID)
	if err != nil {
		return fmt.Errorf("reindex %s: %w", modPath, err)
	}
	type row struct {
		version, synopsis, readme, license, goVersion string
		published                                     int64
	}
	byVersion := map[string]row{}
	var versions []string
	for rows.Next() {
		var v row
		if err := rows.Scan(&v.version, &v.published, &v.synopsis, &v.readme, &v.license, &v.goVersion); err != nil {
			rows.Close()
			return err
		}
		byVersion[v.version] = v
		versions = append(versions, v.version)
	}
	rows.Close()

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM module_meta WHERE module_id = ?`, moduleID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM module_fts WHERE rowid = ?`, moduleID); err != nil {
		return err
	}
	if latest := module.Latest(versions); latest != "" && !quarantined.Valid {
		v := byVersion[latest]
		deprecated := 0
		if deprecation != "" {
			deprecated = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO module_meta (module_id, path, namespace, version, synopsis, license, go_version, go_num,
				published_at, created_at, deprecated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			moduleID, modPath, namespace, latest, v.synopsis, v.license, v.goVersion, GoNum(v.goVersion), v.published, created, deprecated); err != nil {
			return fmt.Errorf("reindex %s: %w", modPath, err)
		}
		prefix, pathMajor, _ := xmodule.SplitPathVersion(modPath)
		name := path.Base(prefix) + " " + strings.TrimPrefix(pathMajor, "/")
		if _, err := tx.ExecContext(ctx, `INSERT INTO module_fts (rowid, path, name, synopsis, readme) VALUES (?, ?, ?, ?, ?)`,
			moduleID, modPath, name, v.synopsis+" "+deprecation, v.readme); err != nil {
			return fmt.Errorf("reindex %s: %w", modPath, err)
		}
	}
	return tx.Commit()
}

func (r *Registry) reindexPath(ctx context.Context, modPath string) {
	var id int64
	if err := r.DB.QueryRowContext(ctx, `SELECT id FROM modules WHERE path = ?`, modPath).Scan(&id); err != nil {
		r.log().Error("reindex: look up module", "module", modPath, "err", err)
		return
	}
	if err := r.reindex(ctx, id); err != nil {
		r.log().Error("reindex", "module", modPath, "err", err)
	}
}

// Backfill extracts search details for versions published before search
// existed, then rebuilds the search index. It is safe to run on every
// start-up; work already done is skipped.
func (r *Registry) Backfill(ctx context.Context) error {
	type pending struct {
		id               int64
		modPath, version string
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT v.id, m.path, v.version FROM versions v JOIN modules m ON m.id = v.module_id WHERE v.indexed = 0`)
	if err != nil {
		return fmt.Errorf("backfill: %w", err)
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.modPath, &p.version); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()

	for _, p := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		fsys, closer, err := r.VersionFS(ctx, p.modPath, p.version)
		if err != nil {
			r.log().Warn("backfill: open version", "module", p.modPath, "version", p.version, "err", err)
			continue
		}
		m := metaFromFS(ctx, fsys, p.modPath)
		closer.Close()
		if _, err := r.DB.ExecContext(ctx, `UPDATE versions SET synopsis = CASE WHEN synopsis = '' THEN ? ELSE synopsis END,
				readme_text = ?, license = ?, go_version = ?, indexed = 1 WHERE id = ?`,
			m.Synopsis, m.Readme, m.License, m.GoVersion, p.id); err != nil {
			return fmt.Errorf("backfill %s@%s: %w", p.modPath, p.version, err)
		}
	}

	// Rebuild entries for every module missing from the index.
	ids, err := r.DB.QueryContext(ctx, `SELECT m.id FROM modules m WHERE NOT EXISTS (SELECT 1 FROM module_meta mm WHERE mm.module_id = m.id)`)
	if err != nil {
		return err
	}
	var missing []int64
	for ids.Next() {
		var id int64
		ids.Scan(&id)
		missing = append(missing, id)
	}
	ids.Close()
	for _, id := range missing {
		if err := r.reindex(ctx, id); err != nil {
			return err
		}
	}
	if len(todo) > 0 || len(missing) > 0 {
		r.log().Info("search index backfilled", "versions", len(todo), "modules", len(missing))
	}
	return nil
}

// ---- Search ----

// Sort orders search results.
type Sort string

const (
	SortRelevance Sort = "relevance"
	SortDownloads Sort = "downloads"
	SortUpdated   Sort = "updated"
	SortNew       Sort = "new"
)

// SearchQuery describes a search. All filters are optional.
type SearchQuery struct {
	Text           string
	License        string        // SPDX ID, e.g. "MIT"
	GoMax          string        // only modules whose go directive is at most this, e.g. "1.22"
	UpdatedWithin  time.Duration // only modules with a release this recent
	HideDeprecated bool
	Sort           Sort
	Limit, Offset  int
}

// SearchHit is one module in the results.
type SearchHit struct {
	Listing
	License     string
	GoVersion   string
	Downloads30 int
	Deprecated  bool
}

// Search finds modules. Words match as prefixes anywhere in the module
// path, name, summary or README, with path and name weighted highest.
func (r *Registry) Search(ctx context.Context, q SearchQuery) ([]SearchHit, int, error) {
	match := ftsQuery(q.Text)
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 20
	}
	since := r.now().AddDate(0, 0, -30).Unix() / 86400

	var where []string
	var args []any
	from := `module_meta mm`
	rank := "0"
	if match != "" {
		from = `module_fts JOIN module_meta mm ON mm.module_id = module_fts.rowid`
		where = append(where, `module_fts MATCH ?`)
		args = append(args, match)
		rank = `bm25(module_fts, 10.0, 12.0, 4.0, 1.0)`
	}
	if q.License != "" {
		where = append(where, `(',' || mm.license || ',') LIKE ?`)
		args = append(args, "%,"+likeEscape(q.License)+",%")
	}
	if n := GoNum(q.GoMax); n > 0 {
		where = append(where, `mm.go_num BETWEEN 1 AND ?`)
		args = append(args, n)
	}
	if q.UpdatedWithin > 0 {
		where = append(where, `mm.published_at >= ?`)
		args = append(args, r.now().Add(-q.UpdatedWithin).Unix())
	}
	if q.HideDeprecated {
		where = append(where, `mm.deprecated = 0`)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+from+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("search: %w", err)
	}

	order := map[Sort]string{
		SortRelevance: `rank, downloads DESC, mm.published_at DESC`,
		SortDownloads: `downloads DESC, rank, mm.published_at DESC`,
		SortUpdated:   `mm.published_at DESC, mm.module_id DESC`,
		SortNew:       `mm.created_at DESC, mm.module_id DESC`,
	}[q.Sort]
	if order == "" || (q.Sort == SortRelevance && match == "") {
		order = `mm.published_at DESC, mm.module_id DESC`
	}
	query := `SELECT mm.path, mm.namespace, mm.version, mm.synopsis, mm.published_at, mm.created_at, mm.license, mm.go_version, mm.deprecated,
			COALESCE((SELECT SUM(d.count) FROM downloads d WHERE d.module_id = mm.module_id AND d.day > ?), 0) AS downloads,
			` + rank + ` AS rank
		FROM ` + from + cond + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	rows, err := r.DB.QueryContext(ctx, query, append(append([]any{since}, args...), q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	hits := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		var published, created int64
		var deprecated int
		var rankValue float64
		if err := rows.Scan(&h.Path, &h.Namespace, &h.Version, &h.Synopsis, &published, &created, &h.License, &h.GoVersion, &deprecated,
			&h.Downloads30, &rankValue); err != nil {
			return nil, 0, fmt.Errorf("search: %w", err)
		}
		h.PublishedAt, h.CreatedAt = time.Unix(published, 0).UTC(), time.Unix(created, 0).UTC()
		h.Deprecated = deprecated == 1
		hits = append(hits, h)
	}
	return hits, total, rows.Err()
}

// ftsQuery turns what someone typed into a safe FTS5 query: every word
// becomes a quoted prefix term, so operators and punctuation in the input
// are treated as text.
func ftsQuery(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var terms []string
	for _, w := range words {
		if len(terms) == 12 {
			break
		}
		terms = append(terms, `"`+w+`"*`)
	}
	return strings.Join(terms, " ")
}

// Facet is a filter value with the number of modules that have it.
type Facet struct {
	Value string
	Count int
}

// Facets lists the licenses and Go versions present, for search filters.
func (r *Registry) Facets(ctx context.Context) (licenses, goVersions []Facet, err error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT license FROM module_meta WHERE license != ''`)
	if err != nil {
		return nil, nil, fmt.Errorf("facets: %w", err)
	}
	counts := map[string]int{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			rows.Close()
			return nil, nil, err
		}
		for _, id := range strings.Split(l, ",") {
			counts[id]++
		}
	}
	rows.Close()
	for v, n := range counts {
		licenses = append(licenses, Facet{v, n})
	}
	slices.SortFunc(licenses, func(a, b Facet) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Value, b.Value)
	})

	rows, err = r.DB.QueryContext(ctx, `SELECT go_num, COUNT(*) FROM module_meta WHERE go_num > 0 GROUP BY go_num ORDER BY go_num DESC`)
	if err != nil {
		return nil, nil, fmt.Errorf("facets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n, c int
		if err := rows.Scan(&n, &c); err != nil {
			return nil, nil, err
		}
		goVersions = append(goVersions, Facet{fmt.Sprintf("%d.%d", n/1000, n%1000), c})
	}
	return licenses, goVersions, rows.Err()
}
