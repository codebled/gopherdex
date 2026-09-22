package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	xmodule "golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/vulndb"
)

// AdvisoryPrefix starts the IDs of advisories published here, e.g.
// GDX-2026-0001, so they never collide with GO-… IDs from vuln.go.dev.
const AdvisoryPrefix = "GDX"

// VersionRange is an affected range: from Introduced (or the first
// version) up to but not including Fixed (or every later version).
type VersionRange struct {
	Introduced string `json:"introduced,omitempty"`
	Fixed      string `json:"fixed,omitempty"`
}

// AffectedPackage narrows an advisory to packages and, optionally, the
// functions or methods in them that are vulnerable.
type AffectedPackage struct {
	Path    string   `json:"path"`
	Symbols []string `json:"symbols,omitempty"`
}

// Advisory is a security advisory for a hosted module.
type Advisory struct {
	ID          string // GDX-2026-0001
	ModulePath  string
	Summary     string
	Details     string
	Aliases     []string
	Ranges      []VersionRange
	Packages    []AffectedPackage
	References  []string
	Credits     string
	CreatedBy   string
	PublishedAt time.Time
	ModifiedAt  time.Time
	WithdrawnAt *time.Time
}

// Affects reports whether version is vulnerable. Withdrawn advisories
// affect nothing.
func (a *Advisory) Affects(version string) bool {
	return a.WithdrawnAt == nil && vulndb.AffectsVersion(a.osvRanges(), version)
}

// Fixed returns the highest fixed version, or "" when there's no fix.
func (a *Advisory) Fixed() string {
	best := ""
	for _, r := range a.Ranges {
		if r.Fixed != "" && (best == "" || semver.Compare(r.Fixed, best) > 0) {
			best = r.Fixed
		}
	}
	return best
}

func (a *Advisory) osvRanges() []vulndb.Range {
	var out []vulndb.Range
	for _, r := range a.Ranges {
		intro := "0"
		if r.Introduced != "" {
			intro = vulndb.Bare(r.Introduced)
		}
		events := []vulndb.Event{{Introduced: intro}}
		if r.Fixed != "" {
			events = append(events, vulndb.Event{Fixed: vulndb.Bare(r.Fixed)})
		}
		out = append(out, vulndb.Range{Type: "SEMVER", Events: events})
	}
	return out
}

// OSV returns the advisory as an entry of the Go vulnerability database.
// pageURL is its page on this site.
func (a *Advisory) OSV(pageURL string) *vulndb.Entry {
	e := &vulndb.Entry{
		SchemaVersion: "1.3.1",
		ID:            a.ID,
		Modified:      a.ModifiedAt,
		Published:     a.PublishedAt,
		Withdrawn:     a.WithdrawnAt,
		Aliases:       a.Aliases,
		Summary:       a.Summary,
		Details:       a.Details,
		Affected: []vulndb.Affected{{
			Package: vulndb.Package{Name: a.ModulePath, Ecosystem: "Go"},
			Ranges:  a.osvRanges(),
		}},
		DatabaseSpecific: &vulndb.DatabaseSpecific{URL: pageURL},
	}
	if len(a.Packages) > 0 {
		es := &vulndb.EcosystemSpecific{}
		for _, p := range a.Packages {
			es.Imports = append(es.Imports, vulndb.Import{Path: p.Path, Symbols: p.Symbols})
		}
		e.Affected[0].EcosystemSpecific = es
	}
	for _, ref := range a.References {
		e.References = append(e.References, vulndb.Reference{Type: "WEB", URL: ref})
	}
	e.References = append(e.References, vulndb.Reference{Type: "ADVISORY", URL: pageURL})
	if a.Credits != "" {
		e.Credits = []vulndb.Credit{{Name: a.Credits}}
	}
	return e
}

// AdvisoryInput is what an owner fills in.
type AdvisoryInput struct {
	Summary    string
	Details    string
	Aliases    string         // "CVE-2026-1234, GHSA-…"
	Ranges     []VersionRange // blank rows are ignored; all blank means every version, no fix yet
	Packages   string         // one per line: "path" or "path: Func, Type.Method"
	References string         // URLs, one per line
	Credits    string
}

var (
	aliasPattern  = regexp.MustCompile(`^(CVE-\d{4}-\d{4,}|GHSA(-[23456789cfghjmpqrvwx]{4}){3})$`)
	symbolPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)
)

// FieldError is a problem with one field of a form.
type FieldError struct{ Field, Message string }

func (e *FieldError) Error() string { return e.Message }

func fieldErr(field, format string, args ...any) error {
	return &FieldError{field, fmt.Sprintf(format, args...)}
}

// normalize checks the input and converts it to stored form.
func (in AdvisoryInput) normalize(modPath string) (*Advisory, error) {
	a := &Advisory{ModulePath: modPath, Summary: strings.TrimSpace(in.Summary), Details: strings.TrimSpace(strings.ReplaceAll(in.Details, "\r\n", "\n")),
		Credits: strings.TrimSpace(in.Credits)}
	switch n := len([]rune(a.Summary)); {
	case n == 0:
		return nil, fieldErr("summary", "Give a one-line summary, like \"Path traversal in retry.Do\".")
	case n > 160 || strings.ContainsAny(a.Summary, "\r\n"):
		return nil, fieldErr("summary", "Keep the summary to one line of up to 160 characters.")
	}
	if a.Details == "" || len(a.Details) > 10_000 {
		return nil, fieldErr("details", "Describe the problem, who is affected and how to fix it, in up to 10,000 characters.")
	}
	if len(a.Credits) > 200 || strings.ContainsAny(a.Credits, "\r\n") {
		return nil, fieldErr("credits", "Keep credits to one line of up to 200 characters.")
	}

	for _, alias := range strings.FieldsFunc(in.Aliases, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		alias = strings.ToUpper(alias)
		if strings.HasPrefix(alias, "GHSA") {
			alias = "GHSA" + strings.ToLower(alias[4:])
		}
		if !aliasPattern.MatchString(alias) {
			return nil, fieldErr("aliases", "%q isn't a CVE (CVE-2026-12345) or GitHub advisory (GHSA-xxxx-xxxx-xxxx) ID.", alias)
		}
		a.Aliases = append(a.Aliases, alias)
	}
	if len(a.Aliases) > 10 {
		return nil, fieldErr("aliases", "Give at most 10 IDs.")
	}

	_, pathMajor, _ := xmodule.SplitPathVersion(modPath)
	version := func(v, what string) (string, error) {
		v = strings.TrimSpace(v)
		if v == "" {
			return "", nil
		}
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		if !semver.IsValid(v) || semver.Canonical(v) != v {
			return "", fieldErr("ranges", "%s version %q isn't a full semantic version like v1.2.3.", what, v)
		}
		if err := xmodule.CheckPathMajor(v, pathMajor); err != nil {
			return "", fieldErr("ranges", "%s is a %s version, which %s can't have.", v, semver.Major(v), modPath)
		}
		return v, nil
	}
	for _, r := range in.Ranges {
		intro, err := version(r.Introduced, "The first affected")
		if err != nil {
			return nil, err
		}
		fixed, err := version(r.Fixed, "The fixed")
		if err != nil {
			return nil, err
		}
		if intro == "" && fixed == "" {
			continue
		}
		if intro != "" && fixed != "" && semver.Compare(intro, fixed) >= 0 {
			return nil, fieldErr("ranges", "The fix (%s) must come after the first affected version (%s).", fixed, intro)
		}
		a.Ranges = append(a.Ranges, VersionRange{Introduced: intro, Fixed: fixed})
	}
	if len(a.Ranges) == 0 {
		a.Ranges = []VersionRange{{}} // every version, no fix yet
	}

	for _, line := range strings.Split(in.Packages, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		path, syms, _ := strings.Cut(line, ":")
		path = strings.Trim(strings.TrimSpace(path), "/")
		switch {
		case path == "" || path == ".":
			path = modPath
		case path != modPath && !strings.HasPrefix(path, modPath+"/"):
			path = modPath + "/" + strings.TrimPrefix(path, "./")
		}
		if err := xmodule.CheckImportPath(path); err != nil {
			return nil, fieldErr("packages", "%q isn't a valid package path in %s.", path, modPath)
		}
		p := AffectedPackage{Path: path}
		for _, sym := range strings.Split(syms, ",") {
			if sym = strings.TrimSpace(sym); sym == "" {
				continue
			}
			if !symbolPattern.MatchString(sym) {
				return nil, fieldErr("packages", "%q isn't a function or method name like Do or Client.Get.", sym)
			}
			p.Symbols = append(p.Symbols, sym)
		}
		a.Packages = append(a.Packages, p)
	}
	if len(a.Packages) > 50 {
		return nil, fieldErr("packages", "List at most 50 packages.")
	}

	for _, line := range strings.Fields(in.References) {
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(line) > 500 {
			return nil, fieldErr("references", "%q isn't an https link.", line)
		}
		a.References = append(a.References, line)
	}
	if len(a.References) > 10 {
		return nil, fieldErr("references", "Give at most 10 links.")
	}
	return a, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// CreateAdvisory publishes an advisory for modPath. Only owners can.
func (r *Registry) CreateAdvisory(ctx context.Context, u *accounts.User, modPath string, in AdvisoryInput, c accounts.Client) (*Advisory, error) {
	moduleID, err := r.requireRole(ctx, u, modPath, RoleOwner)
	if err != nil {
		return nil, err
	}
	a, err := in.normalize(modPath)
	if err != nil {
		return nil, err
	}
	now := r.now().UTC().Truncate(time.Second)
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create advisory: %w", err)
	}
	defer tx.Rollback()
	prefix := fmt.Sprintf("%s-%d-", AdvisoryPrefix, now.Year())
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM advisories WHERE advisory_id LIKE ?`, prefix+"%").Scan(&n); err != nil {
		return nil, fmt.Errorf("create advisory: %w", err)
	}
	a.ID = fmt.Sprintf("%s%04d", prefix, n+1)
	if _, err := tx.ExecContext(ctx, `INSERT INTO advisories (advisory_id, module_id, summary, details, aliases, ranges, packages, refs, credits,
			created_by, published_at, modified_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, moduleID, a.Summary, a.Details, mustJSON(a.Aliases), mustJSON(a.Ranges), mustJSON(a.Packages), mustJSON(a.References), a.Credits,
		u.ID, now.Unix(), now.Unix()); err != nil {
		return nil, fmt.Errorf("create advisory: %w", err)
	}
	if err := r.audit(ctx, tx, u.ID, "advisory.published", a.ID+" "+modPath, c); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create advisory: %w", err)
	}
	a.CreatedBy, a.PublishedAt, a.ModifiedAt = u.Username, now, now
	return a, nil
}

// UpdateAdvisory replaces an advisory's details, for example to add the
// fixed version once it's released.
func (r *Registry) UpdateAdvisory(ctx context.Context, u *accounts.User, id string, in AdvisoryInput, c accounts.Client) (*Advisory, error) {
	old, err := r.Advisory(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := r.requireRole(ctx, u, old.ModulePath, RoleOwner); err != nil {
		return nil, err
	}
	a, err := in.normalize(old.ModulePath)
	if err != nil {
		return nil, err
	}
	now := r.now().UTC().Truncate(time.Second)
	if _, err := r.DB.ExecContext(ctx, `UPDATE advisories SET summary = ?, details = ?, aliases = ?, ranges = ?, packages = ?, refs = ?, credits = ?,
			modified_at = ? WHERE advisory_id = ?`,
		a.Summary, a.Details, mustJSON(a.Aliases), mustJSON(a.Ranges), mustJSON(a.Packages), mustJSON(a.References), a.Credits, now.Unix(), id); err != nil {
		return nil, fmt.Errorf("update advisory: %w", err)
	}
	if err := r.audit(ctx, r.DB, u.ID, "advisory.updated", id, c); err != nil {
		return nil, err
	}
	return r.Advisory(ctx, id)
}

// WithdrawAdvisory marks an advisory as published in error. It stays
// visible, marked withdrawn, and tools stop reporting it.
func (r *Registry) WithdrawAdvisory(ctx context.Context, u *accounts.User, id string, c accounts.Client) (*Advisory, error) {
	a, err := r.Advisory(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := r.requireRole(ctx, u, a.ModulePath, RoleOwner); err != nil {
		return nil, err
	}
	if a.WithdrawnAt != nil {
		return nil, reject(http.StatusConflict, "already_withdrawn", "%s is already withdrawn.", id)
	}
	now := r.now().Unix()
	if _, err := r.DB.ExecContext(ctx, `UPDATE advisories SET withdrawn_at = ?, modified_at = ? WHERE advisory_id = ?`, now, now, id); err != nil {
		return nil, fmt.Errorf("withdraw advisory: %w", err)
	}
	if err := r.audit(ctx, r.DB, u.ID, "advisory.withdrawn", id, c); err != nil {
		return nil, err
	}
	return r.Advisory(ctx, id)
}

const advisoryColumns = `a.advisory_id, m.path, a.summary, a.details, a.aliases, a.ranges, a.packages, a.refs, a.credits,
	COALESCE(u.username, ''), a.published_at, a.modified_at, a.withdrawn_at`

const advisoryFrom = ` FROM advisories a JOIN modules m ON m.id = a.module_id LEFT JOIN users u ON u.id = a.created_by
	WHERE m.quarantined_at IS NULL`

func scanAdvisory(row interface{ Scan(...any) error }) (*Advisory, error) {
	var (
		a                               Advisory
		aliases, ranges, packages, refs string
		published, modified             int64
		withdrawn                       sql.NullInt64
	)
	if err := row.Scan(&a.ID, &a.ModulePath, &a.Summary, &a.Details, &aliases, &ranges, &packages, &refs, &a.Credits,
		&a.CreatedBy, &published, &modified, &withdrawn); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		s   string
		dst any
	}{{aliases, &a.Aliases}, {ranges, &a.Ranges}, {packages, &a.Packages}, {refs, &a.References}} {
		if err := json.Unmarshal([]byte(f.s), f.dst); err != nil {
			return nil, fmt.Errorf("advisory %s: %w", a.ID, err)
		}
	}
	a.PublishedAt, a.ModifiedAt = time.Unix(published, 0).UTC(), time.Unix(modified, 0).UTC()
	if withdrawn.Valid {
		t := time.Unix(withdrawn.Int64, 0).UTC()
		a.WithdrawnAt = &t
	}
	return &a, nil
}

// Advisory returns one advisory by ID.
func (r *Registry) Advisory(ctx context.Context, id string) (*Advisory, error) {
	a, err := scanAdvisory(r.DB.QueryRowContext(ctx, `SELECT `+advisoryColumns+advisoryFrom+` AND a.advisory_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("advisory %s: %w", id, module.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load advisory %s: %w", id, err)
	}
	return a, nil
}

func (r *Registry) advisories(ctx context.Context, where string, args ...any) ([]*Advisory, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+advisoryColumns+advisoryFrom+where+` ORDER BY a.published_at DESC, a.id DESC`, args...) //nolint:gosec // constant fragments; callers pass constant where clauses with values as ? args
	if err != nil {
		return nil, fmt.Errorf("list advisories: %w", err)
	}
	defer rows.Close()
	var out []*Advisory
	for rows.Next() {
		a, err := scanAdvisory(rows)
		if err != nil {
			return nil, fmt.Errorf("list advisories: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ModuleAdvisories lists a module's advisories, newest first, withdrawn
// ones included.
func (r *Registry) ModuleAdvisories(ctx context.Context, modPath string) ([]*Advisory, error) {
	return r.advisories(ctx, ` AND m.path = ?`, modPath)
}

// AllAdvisories lists every advisory, newest first.
func (r *Registry) AllAdvisories(ctx context.Context) ([]*Advisory, error) {
	return r.advisories(ctx, "")
}

// String describes the range for people, e.g. "v1.1.0 up to v1.2.0".
func (r VersionRange) String() string {
	switch {
	case r.Introduced == "" && r.Fixed == "":
		return "every version (no fix yet)"
	case r.Introduced == "":
		return "every version before " + r.Fixed
	case r.Fixed == "":
		return r.Introduced + " and later (no fix yet)"
	default:
		return r.Introduced + " up to " + r.Fixed + " (not included)"
	}
}
