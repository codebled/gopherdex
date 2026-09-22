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

// indexedVersion is what the search index keeps of one release.
type indexedVersion struct {
	version, synopsis, readme, license, goVersion string
	published                                     int64
}

// indexedVersions loads the unyanked releases of a module for reindex,
// keyed by version, along with the list of versions. The rows are closed
// before reindex opens its transaction.
func (r *Registry) indexedVersions(ctx context.Context, moduleID int64) (map[string]indexedVersion, []string, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT version, published_at, synopsis, readme_text, license, go_version
		FROM versions WHERE module_id = ? AND yanked_at IS NULL`, moduleID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byVersion := map[string]indexedVersion{}
	var versions []string
	for rows.Next() {
		var v indexedVersion
		if err := rows.Scan(&v.version, &v.published, &v.synopsis, &v.readme, &v.license, &v.goVersion); err != nil {
			return nil, nil, err
		}
		byVersion[v.version] = v
		versions = append(versions, v.version)
	}
	return byVersion, versions, rows.Err()
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
	byVersion, versions, err := r.indexedVersions(ctx, moduleID)
	if err != nil {
		return fmt.Errorf("reindex %s: %w", modPath, err)
	}

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
				published_at, created_at, deprecated, downloads_30d) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
				COALESCE((SELECT SUM(count) FROM downloads WHERE module_id = ? AND day > ?), 0))`,
			moduleID, modPath, namespace, latest, v.synopsis, v.license, v.goVersion, GoNum(v.goVersion), v.published, created, deprecated,
			moduleID, r.downloadWindowStart()); err != nil {
			return fmt.Errorf("reindex %s: %w", modPath, err)
		}
		prefix, pathMajor, _ := xmodule.SplitPathVersion(modPath)
		name := path.Base(prefix) + " " + strings.TrimPrefix(pathMajor, "/")
		// The path is indexed without the registry's host, which every
		// module shares: otherwise "go" or "dev" would match them all.
		_, indexedPath, _ := strings.Cut(modPath, "/")
		if _, err := tx.ExecContext(ctx, `INSERT INTO module_fts (rowid, path, name, synopsis, readme) VALUES (?, ?, ?, ?, ?)`,
			moduleID, indexedPath, name, v.synopsis+" "+deprecation, v.readme); err != nil {
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
	todo, err := r.unindexedVersions(ctx)
	if err != nil {
		return fmt.Errorf("backfill: %w", err)
	}

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
	missing, err := r.unindexedModules(ctx)
	if err != nil {
		return err
	}
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

// pendingIndex is a version whose search details aren't extracted yet.
type pendingIndex struct {
	id               int64
	modPath, version string
}

// unindexedVersions lists the versions Backfill still has to extract
// search details from, read in full so the rows are closed before it
// writes.
func (r *Registry) unindexedVersions(ctx context.Context) ([]pendingIndex, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT v.id, m.path, v.version FROM versions v JOIN modules m ON m.id = v.module_id WHERE v.indexed = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var todo []pendingIndex
	for rows.Next() {
		var p pendingIndex
		if err := rows.Scan(&p.id, &p.modPath, &p.version); err != nil {
			return nil, err
		}
		todo = append(todo, p)
	}
	return todo, rows.Err()
}

// unindexedModules lists the modules with no search index entry, read in
// full so Backfill can reindex them without holding the rows open.
func (r *Registry) unindexedModules(ctx context.Context) ([]int64, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT m.id FROM modules m WHERE NOT EXISTS (SELECT 1 FROM module_meta mm WHERE mm.module_id = m.id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var missing []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		missing = append(missing, id)
	}
	return missing, rows.Err()
}

// ---- Search ----

// Sort orders search results.
type Sort string

// The search orders. Without search text, relevance falls back to
// SortUpdated.
const (
	SortRelevance Sort = "relevance" // best text match first: name, then synopsis, then the rest
	SortDownloads Sort = "downloads" // most downloads in the last 30 days first
	SortUpdated   Sort = "updated"   // most recent release first
	SortNew       Sort = "new"       // most recently created module first
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
// path, name, summary or README.
//
// By relevance, modules whose path or name match come first, then those
// that match in their summary, then those that match only in their README,
// each tier ranked by BM25 with ties going to the more downloaded module.
// A tier is only ranked when a page reaches it, which keeps broad queries
// cheap: "go" appears in most summaries, but in far fewer names.
func (r *Registry) Search(ctx context.Context, q SearchQuery) ([]SearchHit, int, error) {
	match := ftsQuery(r.searchText(q.Text))
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 20
	}
	var filters []string
	var args []any
	if q.License != "" {
		filters = append(filters, `(',' || mm.license || ',') LIKE ?`)
		args = append(args, "%,"+likeEscape(q.License)+",%")
	}
	if n := GoNum(q.GoMax); n > 0 {
		filters = append(filters, `mm.go_num BETWEEN 1 AND ?`)
		args = append(args, n)
	}
	if q.UpdatedWithin > 0 {
		filters = append(filters, `mm.published_at >= ?`)
		args = append(args, r.now().Add(-q.UpdatedWithin).Unix())
	}
	if q.HideDeprecated {
		filters = append(filters, `mm.deprecated = 0`)
	}
	// where builds a WHERE clause, optionally with an FTS match first.
	where := func(fts string) (string, []any) {
		conds, all := filters, args
		if fts != "" {
			conds = append([]string{`module_fts MATCH ?`}, filters...)
			all = append([]any{fts}, args...)
		}
		if len(conds) == 0 {
			return "", all
		}
		return " WHERE " + strings.Join(conds, " AND "), all
	}
	const ftsFrom = `module_fts JOIN module_meta mm ON mm.module_id = module_fts.rowid`
	count := func(fts string) (int, error) {
		from := `module_meta mm`
		switch {
		case fts != "" && len(filters) == 0:
			// Every indexed module has its details row, so without filters
			// the full-text index counts on its own, skipping the join.
			var n int
			err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM module_fts WHERE module_fts MATCH ?`, fts).Scan(&n)
			return n, err
		case fts != "":
			from = ftsFrom
		}
		cond, a := where(fts)
		var n int
		err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+from+cond, a...).Scan(&n)
		return n, err
	}
	page := func(fts, order string, limit, offset int) ([]SearchHit, error) {
		from := `module_meta mm`
		if fts != "" {
			from = ftsFrom
		}
		cond, a := where(fts)
		return r.searchRows(ctx, `SELECT mm.path, mm.namespace, mm.version, mm.synopsis, mm.published_at, mm.created_at,
				mm.license, mm.go_version, mm.deprecated, mm.downloads_30d
			FROM `+from+cond+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(a, limit, offset)...)
	}

	total, err := count(match)
	if err != nil {
		return nil, 0, fmt.Errorf("search: %w", err)
	}
	if match != "" && (q.Sort == SortRelevance || q.Sort == "") {
		const byRank = `bm25(module_fts, 10.0, 12.0, 4.0, 1.0), mm.downloads_30d DESC, mm.published_at DESC`
		named := `{path name} : (` + match + `)`
		tiers := []string{
			named,
			`({synopsis} : (` + match + `)) NOT (` + named + `)`,
			`(` + match + `) NOT ({path name synopsis} : (` + match + `))`,
		}
		hits := []SearchHit{}
		skip := q.Offset
		for _, tier := range tiers {
			if skip > 0 {
				// Count a tier only to page past it.
				n, err := count(tier)
				if err != nil {
					return nil, 0, fmt.Errorf("search: %w", err)
				}
				if skip >= n {
					skip -= n
					continue
				}
			}
			more, err := page(tier, byRank, q.Limit-len(hits), skip)
			if err != nil {
				return nil, 0, err
			}
			hits, skip = append(hits, more...), 0
			if len(hits) == q.Limit {
				break
			}
		}
		return hits, total, nil
	}

	order := map[Sort]string{
		SortDownloads: `mm.downloads_30d DESC, mm.published_at DESC, mm.module_id DESC`,
		SortUpdated:   `mm.published_at DESC, mm.module_id DESC`,
		SortNew:       `mm.created_at DESC, mm.module_id DESC`,
	}[q.Sort]
	if order == "" {
		order = `mm.published_at DESC, mm.module_id DESC`
	}
	hits, err := page(match, order, q.Limit, q.Offset)
	return hits, total, err
}

// searchText drops a leading URL scheme and the registry's own host, so
// pasting a full module path finds the module: paths are indexed without
// the host.
func (r *Registry) searchText(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "https://"), "http://")
	if r.ModuleHost != "" {
		if rest, ok := strings.CutPrefix(t, r.ModuleHost); ok && (rest == "" || rest[0] == '/') {
			t = strings.TrimPrefix(rest, "/")
		}
	}
	return t
}

func (r *Registry) searchRows(ctx context.Context, query string, args ...any) ([]SearchHit, error) {
	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	hits := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		var published, created int64
		var deprecated int
		if err := rows.Scan(&h.Path, &h.Namespace, &h.Version, &h.Synopsis, &published, &created, &h.License, &h.GoVersion, &deprecated,
			&h.Downloads30); err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		h.PublishedAt, h.CreatedAt = time.Unix(published, 0).UTC(), time.Unix(created, 0).UTC()
		h.Deprecated = deprecated == 1
		hits = append(hits, h)
	}
	return hits, rows.Err()
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
	counts, err := r.licenseCounts(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("facets: %w", err)
	}
	for v, n := range counts {
		licenses = append(licenses, Facet{v, n})
	}
	slices.SortFunc(licenses, func(a, b Facet) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Value, b.Value)
	})

	rows, err := r.DB.QueryContext(ctx, `SELECT go_num, COUNT(*) FROM module_meta WHERE go_num > 0 GROUP BY go_num ORDER BY go_num DESC`)
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

// licenseCounts counts the modules under each license in the search
// index. A module with several licenses counts toward each of them.
func (r *Registry) licenseCounts(ctx context.Context) (map[string]int, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT license FROM module_meta WHERE license != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		for _, id := range strings.Split(l, ",") {
			counts[id]++
		}
	}
	return counts, rows.Err()
}
