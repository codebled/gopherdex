// Package registry publishes modules uploaded by their owners and serves
// them back to the go command and the discovery pages.
//
// Module paths look like <host>/<namespace>/<name>[/vN]. Only the owner of a
// namespace can publish under it, every version is immutable, and each zip
// is checked with the same rules the go command applies before it is stored.
package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"
	xmodule "golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/blob"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/scan"
)

// DefaultMaxZipSize caps uploads. The go command accepts up to 500 MiB, but
// a public registry has no reason to accept that much by default.
const DefaultMaxZipSize = 50 << 20

// Error is a publish failure caused by the request, with a message for the
// person publishing.
type Error struct {
	Status  int    // HTTP status
	Code    string // stable identifier for tools
	Message string
}

func (e *Error) Error() string { return e.Message }

func reject(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Registry stores and serves published modules.
type Registry struct {
	// popular caches the download ranking the name checks compare against.
	popularMu sync.Mutex
	popular   []popularModule
	popularAt time.Time

	DB         *sql.DB
	Blobs      blob.Store
	ModuleHost string // e.g. "gopherdex.dev"
	MaxZipSize int64
	Log        *slog.Logger
	Now        func() time.Time
	// Require2FA refuses uploads from accounts without two-factor
	// authentication, as PyPI does.
	Require2FA bool
	// SkipChecks turns off the publish-time checks (package scan).
	SkipChecks bool
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Registry) maxZip() int64 {
	if r.MaxZipSize > 0 {
		return r.MaxZipSize
	}
	return DefaultMaxZipSize
}

// Upload is one publish request. ZipFile is a path to the uploaded zip on
// local disk.
type Upload struct {
	User       *accounts.User
	Token      *accounts.Token
	Client     accounts.Client
	Module     string
	Version    string
	ZipFile    string
	Repository string // optional https URL of the source repository
	Commit     string // optional commit hash the version was built from
	Ref        string // optional, e.g. refs/tags/v1.2.0
}

// Published describes a stored version.
type Published struct {
	Module           string    `json:"module"`
	Version          string    `json:"version"`
	H1               string    `json:"h1"`
	GoModH1          string    `json:"goModH1"`
	SHA256           string    `json:"sha256"`
	Size             int64     `json:"size"`
	PublishedAt      time.Time `json:"publishedAt"`
	AlreadyPublished bool      `json:"alreadyPublished"`
	// Warnings are what the publish checks flagged; the version is
	// published and administrators review them.
	Warnings []scan.Finding `json:"warnings,omitempty"`
}

// Module names are lower-case so paths never need case escaping and can't
// be confused with each other.
var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9])?$`)

// CheckModulePath reports whether path is a valid registry module path and
// returns its namespace.
func (r *Registry) CheckModulePath(path string) (namespace string, err error) {
	if err := xmodule.CheckPath(path); err != nil {
		return "", reject(http.StatusBadRequest, "invalid_module_path", "%v", err)
	}
	rest, ok := strings.CutPrefix(path, r.ModuleHost+"/")
	if !ok {
		return "", reject(http.StatusBadRequest, "wrong_host",
			"Module paths on this registry start with %s/<your-username>/. Rename the module in go.mod, for example to %s/<your-username>/%s.",
			r.ModuleHost, r.ModuleHost, lastElem(path))
	}
	prefix, pathMajor, _ := xmodule.SplitPathVersion(rest)
	elems := strings.Split(prefix, "/")
	if len(elems) != 2 {
		return "", reject(http.StatusBadRequest, "invalid_module_path",
			"Module paths must be %s/<namespace>/<name>, with an optional /vN major version suffix. %q has %d parts after the host.",
			r.ModuleHost, path, len(elems))
	}
	if !namePattern.MatchString(elems[1]) {
		return "", reject(http.StatusBadRequest, "invalid_module_name",
			"Module name %q must be lower-case letters, digits, '.', '_' or '-', starting and ending with a letter or digit.", elems[1])
	}
	if pathMajor != "" && !strings.HasPrefix(pathMajor, "/v") {
		return "", reject(http.StatusBadRequest, "invalid_module_path", "Unsupported major version suffix %q.", pathMajor)
	}
	return elems[0], nil
}

func lastElem(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Publish validates and stores an uploaded version.
func (r *Registry) Publish(ctx context.Context, u Upload) (*Published, error) {
	namespace, err := r.CheckModulePath(u.Module)
	if err != nil {
		return nil, err
	}
	if u.User == nil || u.Token == nil {
		return nil, reject(http.StatusUnauthorized, "unauthenticated", "Publishing needs an API token.")
	}
	if err := TokenAllows(u.Token, u.User, u.Module); err != nil {
		return nil, err
	}
	if r.Require2FA && !u.User.TwoFactor {
		return nil, reject(http.StatusForbidden, "2fa_required",
			"This registry requires two-factor authentication to publish. Turn it on at %s/account/security, then publish again.", "your account's security page")
	}
	if q, err := r.quarantined(ctx, u.Module); err != nil {
		return nil, err
	} else if q {
		return nil, reject(http.StatusForbidden, "quarantined", "%s is under review by the registry's administrators and can't receive new versions right now.", u.Module)
	}
	if err := r.canPublish(ctx, u.User, u.Module, namespace); err != nil {
		return nil, err
	}
	if !u.User.EmailVerified {
		return nil, reject(http.StatusForbidden, "email_not_verified", "Verify your email address before publishing.")
	}
	if err := checkVersion(u.Module, u.Version); err != nil {
		return nil, err
	}
	if err := checkOrigin(&u); err != nil {
		return nil, err
	}
	// A trusted publisher's release records the CI run that built it, and
	// its repository is the one GitHub vouched for, not what the client said.
	provenance := ""
	if u.Token.PublisherID != 0 {
		prov := ParseProvenance(u.Token.Claims)
		if prov == nil {
			return nil, fmt.Errorf("publisher token %d has no provenance", u.Token.ID)
		}
		provenance, u.Repository = u.Token.Claims, prov.RepositoryURL()
	}

	st, err := os.Stat(u.ZipFile)
	if err != nil {
		return nil, fmt.Errorf("stat upload: %w", err)
	}
	if st.Size() > r.maxZip() {
		return nil, reject(http.StatusRequestEntityTooLarge, "zip_too_large", "The module zip is %d bytes; the limit is %d bytes.", st.Size(), r.maxZip())
	}

	mv := xmodule.Version{Path: u.Module, Version: u.Version}
	if _, err := modzip.CheckZip(mv, u.ZipFile); err != nil {
		return nil, reject(http.StatusUnprocessableEntity, "invalid_zip", "The module zip isn't valid: %v", err)
	}
	goMod, err := readGoMod(u.ZipFile, mv)
	if err != nil {
		return nil, err
	}
	h1, err := dirhash.HashZip(u.ZipFile, dirhash.Hash1)
	if err != nil {
		return nil, reject(http.StatusUnprocessableEntity, "invalid_zip", "The module zip couldn't be hashed: %v", err)
	}
	goModH1, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(goMod)), nil
	})
	if err != nil {
		return nil, fmt.Errorf("hash go.mod: %w", err)
	}

	meta := zipMeta(ctx, u.ZipFile, mv)

	if prev, err := r.publishedVersion(ctx, u.Module, u.Version); err == nil {
		return sameOrConflict(prev, h1)
	} else if !errors.Is(err, module.ErrNotFound) {
		return nil, err
	}

	var findings []scan.Finding
	if !r.SkipChecks {
		if findings, err = r.runChecks(ctx, u, namespace); err != nil {
			return nil, err
		}
	}

	f, err := os.Open(u.ZipFile)
	if err != nil {
		return nil, fmt.Errorf("open upload: %w", err)
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return nil, fmt.Errorf("hash upload: %w", err)
	}
	sha := hex.EncodeToString(sum.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	// Keys include the content hash, so a retried upload that died after
	// storing the blob finds identical bytes already in place.
	escPath, _ := module.EscapePath(u.Module)
	escVersion, _ := module.EscapeVersion(u.Version)
	key := "modules/" + escPath + "/@v/" + escVersion + "-" + sha[:16] + ".zip"
	if _, err := r.Blobs.Put(ctx, key, f, r.maxZip()); err != nil && !errors.Is(err, blob.ErrExists) {
		return nil, fmt.Errorf("store zip for %s@%s: %w", u.Module, u.Version, err)
	}

	now := r.now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("publish %s@%s: %w", u.Module, u.Version, err)
	}
	defer tx.Rollback()

	created, err := tx.ExecContext(ctx, `INSERT INTO modules (path, namespace, created_by, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (path) DO NOTHING`, u.Module, namespace, u.User.ID, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("publish %s: create module: %w", u.Module, err)
	}
	var moduleID int64
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT id, namespace FROM modules WHERE path = ?`, u.Module).Scan(&moduleID, &owner); err != nil {
		return nil, fmt.Errorf("publish %s: %w", u.Module, err)
	}
	if owner != namespace {
		return nil, fmt.Errorf("module %s belongs to namespace %q, not %q", u.Module, owner, namespace)
	}
	// The first publisher of a module in a personal namespace owns it.
	// Organization modules are managed through the organization's roles,
	// except that where members get no access without a team, a member who
	// creates a module maintains it until an owner decides otherwise.
	if n, _ := created.RowsAffected(); n == 1 {
		role := ""
		if namespace == u.User.Username {
			role = "owner"
		} else if err := tx.QueryRowContext(ctx, `SELECT 'maintainer' FROM organizations o JOIN org_members om ON om.org_id = o.id
				WHERE o.name = ? AND om.user_id = ? AND om.role = 'member' AND o.member_access = 'none'`,
			namespace, u.User.ID).Scan(&role); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("publish %s: %w", u.Module, err)
		}
		if role != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO module_roles (module_id, user_id, role, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
				moduleID, u.User.ID, role, u.User.ID, now.Unix()); err != nil {
				return nil, fmt.Errorf("publish %s: record %s: %w", u.Module, role, err)
			}
		}
	}
	vcs := ""
	if u.Commit != "" {
		vcs = "git"
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO versions (module_id, version, go_mod, zip_key, zip_size, zip_sha256, h1, go_mod_h1,
			vcs, repository, commit_hash, ref, published_by, token_id, published_at,
			synopsis, readme_text, license, go_version, indexed, provenance, namespace)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?) ON CONFLICT (module_id, version) DO NOTHING`,
		moduleID, u.Version, goMod, key, st.Size(), sha, h1, goModH1,
		vcs, u.Repository, u.Commit, u.Ref, u.User.ID, u.Token.ID, now.Unix(),
		meta.Synopsis, meta.Readme, meta.License, meta.GoVersion, provenance, namespace)
	if err != nil {
		return nil, fmt.Errorf("publish %s@%s: %w", u.Module, u.Version, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Someone published the same version between our check and insert.
		tx.Rollback()
		prev, err := r.publishedVersion(ctx, u.Module, u.Version)
		if err != nil {
			return nil, err
		}
		return sameOrConflict(prev, h1)
	}
	versionID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("publish %s@%s: %w", u.Module, u.Version, err)
	}
	if err := recordRequires(ctx, tx, versionID, goMod); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (user_id, action, detail, ip, created_at) VALUES (?, 'module.published', ?, ?, ?)`,
		u.User.ID, fmt.Sprintf("%s@%s token=%d %s", u.Module, u.Version, u.Token.ID, h1), u.Client.IP, now.Unix()); err != nil {
		return nil, fmt.Errorf("audit publish: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("publish %s@%s: %w", u.Module, u.Version, err)
	}
	if err := r.reindex(ctx, moduleID); err != nil {
		r.log().Error("reindex after publish", "module", u.Module, "err", err)
	}
	if err := r.recordFindings(ctx, moduleID, versionID, u.Module, u.Version, findings); err != nil {
		r.log().Error("record publish findings", "module", u.Module, "version", u.Version, "err", err)
	}
	r.log().Info("module published", "module", u.Module, "version", u.Version, "user", u.User.Username, "h1", h1)
	return &Published{
		Module: u.Module, Version: u.Version, H1: h1, GoModH1: goModH1,
		SHA256: sha, Size: st.Size(), PublishedAt: now.UTC().Truncate(time.Second), Warnings: findings,
	}, nil
}

func (r *Registry) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func sameOrConflict(prev *versionRow, h1 string) (*Published, error) {
	if prev.h1 != h1 {
		return nil, reject(http.StatusConflict, "version_exists",
			"%s@%s is already published with different content (%s). Published versions can't be replaced; tag and publish a new version instead.",
			prev.module, prev.version, prev.h1)
	}
	return &Published{
		Module: prev.module, Version: prev.version, H1: prev.h1, GoModH1: prev.goModH1,
		SHA256: prev.sha256, Size: prev.size, PublishedAt: prev.publishedAt, AlreadyPublished: true,
	}, nil
}

func checkVersion(modPath, v string) error {
	if !semver.IsValid(v) || semver.Canonical(v) != v || strings.Contains(v, "+") {
		return reject(http.StatusBadRequest, "invalid_version",
			"Version %q must be a full semantic version like v1.2.3 or v1.2.3-rc.1.", v)
	}
	if xmodule.IsPseudoVersion(v) {
		return reject(http.StatusBadRequest, "invalid_version",
			"%s is a pseudo-version. Publish tagged releases only: git tag v1.2.3.", v)
	}
	_, pathMajor, _ := xmodule.SplitPathVersion(modPath)
	if err := xmodule.CheckPathMajor(v, pathMajor); err != nil {
		want := semver.Major(v)
		if want == "v0" || want == "v1" {
			return reject(http.StatusBadRequest, "major_version_mismatch",
				"%s is a %s version, so the module path must not end in %s.", v, want, pathMajor)
		}
		return reject(http.StatusBadRequest, "major_version_mismatch",
			"%s is a %s version, so the module path must end in /%s (Go's major version rule). Change go.mod to module %s/%s.",
			v, want, want, strings.TrimSuffix(modPath, pathMajor), want)
	}
	return nil
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func checkOrigin(u *Upload) error {
	if u.Commit != "" && !commitPattern.MatchString(u.Commit) {
		return reject(http.StatusBadRequest, "invalid_commit", "Commit must be a lower-case hex hash.")
	}
	if len(u.Ref) > 256 || strings.ContainsFunc(u.Ref, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return reject(http.StatusBadRequest, "invalid_ref", "Ref must be a short git ref like refs/tags/v1.2.3.")
	}
	if u.Repository != "" {
		repo, err := url.Parse(u.Repository)
		if err != nil || repo.Scheme != "https" || repo.Host == "" || repo.User != nil || len(u.Repository) > 512 {
			return reject(http.StatusBadRequest, "invalid_repository", "Repository must be an https URL without credentials.")
		}
	}
	return nil
}

func readGoMod(zipFile string, mv xmodule.Version) ([]byte, error) {
	zr, err := zip.OpenReader(zipFile)
	if err != nil {
		return nil, reject(http.StatusUnprocessableEntity, "invalid_zip", "The module zip can't be opened: %v", err)
	}
	defer zr.Close()
	name := mv.Path + "@" + mv.Version + "/go.mod"
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, reject(http.StatusUnprocessableEntity, "invalid_zip", "go.mod can't be read: %v", err)
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, 16<<20+1))
		if err != nil {
			return nil, reject(http.StatusUnprocessableEntity, "invalid_zip", "go.mod can't be read: %v", err)
		}
		mf, err := modfile.ParseLax("go.mod", data, nil)
		if err != nil {
			return nil, reject(http.StatusUnprocessableEntity, "invalid_go_mod", "go.mod has errors: %v", err)
		}
		if mf.Module == nil || mf.Module.Mod.Path != mv.Path {
			declared := ""
			if mf.Module != nil {
				declared = mf.Module.Mod.Path
			}
			return nil, reject(http.StatusUnprocessableEntity, "go_mod_path_mismatch",
				"go.mod declares module %q, but you're publishing %q.", declared, mv.Path)
		}
		return data, nil
	}
	return nil, reject(http.StatusUnprocessableEntity, "missing_go_mod", "The module zip has no go.mod at its root. Run go mod init %s first.", mv.Path)
}

// ---- Reading published modules ----

type versionRow struct {
	module, version, h1, goModH1, sha256, zipKey string
	vcs, repository, commit, ref                 string
	size                                         int64
	publishedAt                                  time.Time
}

func (r *Registry) publishedVersion(ctx context.Context, modPath, version string) (*versionRow, error) {
	var v versionRow
	var published int64
	err := r.DB.QueryRowContext(ctx, `SELECT m.path, v.version, v.h1, v.go_mod_h1, v.zip_sha256, v.zip_key, v.vcs, v.repository,
			v.commit_hash, v.ref, v.zip_size, v.published_at
		FROM versions v JOIN modules m ON m.id = v.module_id WHERE m.path = ? AND v.version = ? AND m.quarantined_at IS NULL`, modPath, version).
		Scan(&v.module, &v.version, &v.h1, &v.goModH1, &v.sha256, &v.zipKey, &v.vcs, &v.repository, &v.commit, &v.ref, &v.size, &published)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s@%s: %w", modPath, version, module.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load %s@%s: %w", modPath, version, err)
	}
	v.publishedAt = time.Unix(published, 0).UTC()
	return &v, nil
}

// Modules lists every module with at least one published version.
func (r *Registry) Modules(ctx context.Context) ([]string, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT m.path FROM modules m WHERE EXISTS (SELECT 1 FROM versions v WHERE v.module_id = m.id) ORDER BY m.path`)
	if err != nil {
		return nil, fmt.Errorf("list modules: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("list modules: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// Versions lists a module's installable versions, lowest first: every
// published version except yanked ones. A module whose versions are all
// yanked still exists and returns an empty list.
func (r *Registry) Versions(ctx context.Context, modPath string) ([]string, error) {
	if err := module.CheckPath(modPath); err != nil {
		return nil, err
	}
	var moduleID int64
	var quarantined sql.NullInt64
	err := r.DB.QueryRowContext(ctx, `SELECT m.id, m.quarantined_at FROM modules m WHERE m.path = ? AND EXISTS (SELECT 1 FROM versions v WHERE v.module_id = m.id)`, modPath).
		Scan(&moduleID, &quarantined)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("module %s: %w", modPath, module.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", modPath, err)
	}
	if quarantined.Valid {
		return nil, fmt.Errorf("%s: %w", modPath, ErrQuarantined)
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT version FROM versions WHERE module_id = ? AND yanked_at IS NULL`, moduleID)
	if err != nil {
		return nil, fmt.Errorf("list versions of %s: %w", modPath, err)
	}
	defer rows.Close()
	versions := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("list versions of %s: %w", modPath, err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	module.Sort(versions)
	return versions, nil
}

// Info returns a version's publish time and origin.
func (r *Registry) Info(ctx context.Context, modPath, version string) (module.Info, error) {
	v, err := r.publishedVersion(ctx, modPath, version)
	if err != nil {
		return module.Info{}, err
	}
	info := module.Info{Version: v.version, Time: v.publishedAt}
	if v.commit != "" || v.repository != "" {
		info.Origin = &module.Origin{VCS: v.vcs, URL: v.repository, Hash: v.commit, Ref: v.ref}
	}
	return info, nil
}

// Latest returns the version the go command resolves @latest to.
func (r *Registry) Latest(ctx context.Context, modPath string) (module.Info, error) {
	versions, err := r.Versions(ctx, modPath)
	if err != nil {
		return module.Info{}, err
	}
	latest := module.Latest(versions)
	if latest == "" {
		return module.Info{}, fmt.Errorf("module %s has no installable versions (all are yanked): %w", modPath, module.ErrNotFound)
	}
	return r.Info(ctx, modPath, latest)
}

// GoMod returns a version's go.mod file.
func (r *Registry) GoMod(ctx context.Context, modPath, version string) ([]byte, error) {
	var data []byte
	err := r.DB.QueryRowContext(ctx, `SELECT v.go_mod FROM versions v JOIN modules m ON m.id = v.module_id WHERE m.path = ? AND v.version = ? AND m.quarantined_at IS NULL`,
		modPath, version).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s@%s: %w", modPath, version, module.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load go.mod of %s@%s: %w", modPath, version, err)
	}
	return data, nil
}

// Zip returns the stored module zip, ready to stream.
func (r *Registry) Zip(ctx context.Context, modPath, version string) (io.WriterTo, error) {
	v, err := r.publishedVersion(ctx, modPath, version)
	if err != nil {
		return nil, err
	}
	rd, err := r.Blobs.Open(ctx, v.zipKey)
	if err != nil {
		return nil, fmt.Errorf("open zip of %s@%s: %w", modPath, version, err)
	}
	return &blobZip{r: rd}, nil
}

type blobZip struct{ r blob.Reader }

func (z *blobZip) WriteTo(w io.Writer) (int64, error) {
	defer z.r.Close()
	return io.Copy(w, z.r)
}

// VersionFS exposes a published version's files, read straight from its
// zip. Close the returned io.Closer when done.
func (r *Registry) VersionFS(ctx context.Context, modPath, version string) (fs.FS, io.Closer, error) {
	v, err := r.publishedVersion(ctx, modPath, version)
	if err != nil {
		return nil, nil, err
	}
	rd, err := r.Blobs.Open(ctx, v.zipKey)
	if err != nil {
		return nil, nil, fmt.Errorf("open zip of %s@%s: %w", modPath, version, err)
	}
	zr, err := zip.NewReader(rd, rd.Size())
	if err != nil {
		rd.Close()
		return nil, nil, fmt.Errorf("read zip of %s@%s: %w", modPath, version, err)
	}
	sub, err := fs.Sub(zr, modPath+"@"+version)
	if err != nil {
		rd.Close()
		return nil, nil, err
	}
	return sub, rd, nil
}

// ModuleSummary is a module listed on its owner's account page.
type ModuleSummary struct {
	Path        string
	Latest      string
	Versions    int
	PublishedAt time.Time
}

// NamespaceModules lists the modules in a namespace, sorted by path.
func (r *Registry) NamespaceModules(ctx context.Context, namespace string) ([]ModuleSummary, error) {
	return r.namespaceModules(ctx, namespace, -1, 0)
}

// NamespaceModulesPage is one page of NamespaceModules, and how many
// modules there are in all. Busy namespaces have thousands.
func (r *Registry) NamespaceModulesPage(ctx context.Context, namespace string, limit, offset int) ([]ModuleSummary, int, error) {
	var total int
	if err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM modules WHERE namespace = ? AND quarantined_at IS NULL`, namespace).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count modules of %s: %w", namespace, err)
	}
	mods, err := r.namespaceModules(ctx, namespace, limit, offset)
	return mods, total, err
}

// namespaceModules lists a namespace's modules by path; limit < 0 means all.
func (r *Registry) namespaceModules(ctx context.Context, namespace string, limit, offset int) ([]ModuleSummary, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT m.path, v.version, v.published_at
		FROM modules m JOIN versions v ON v.module_id = m.id
		WHERE m.id IN (SELECT id FROM modules WHERE namespace = ? AND quarantined_at IS NULL ORDER BY path LIMIT ? OFFSET ?)
		ORDER BY m.path`, namespace, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list modules of %s: %w", namespace, err)
	}
	defer rows.Close()
	var out []ModuleSummary
	var versions []string
	flush := func() {
		if len(out) > 0 {
			out[len(out)-1].Latest = module.Latest(versions)
		}
		versions = nil
	}
	for rows.Next() {
		var p, v string
		var published int64
		if err := rows.Scan(&p, &v, &published); err != nil {
			return nil, fmt.Errorf("list modules of %s: %w", namespace, err)
		}
		if len(out) == 0 || out[len(out)-1].Path != p {
			flush()
			out = append(out, ModuleSummary{Path: p})
		}
		cur := &out[len(out)-1]
		cur.Versions++
		if t := time.Unix(published, 0); t.After(cur.PublishedAt) {
			cur.PublishedAt = t
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

// ModuleRoot returns the published module that contains importPath, which
// may be the module itself or a package inside it. A trailing /vN element
// is tried as a major version module first.
func (r *Registry) ModuleRoot(ctx context.Context, importPath string) (string, error) {
	rest, ok := strings.CutPrefix(importPath, r.ModuleHost+"/")
	if !ok {
		return "", fmt.Errorf("%s: %w", importPath, module.ErrNotFound)
	}
	elems := strings.Split(rest, "/")
	if len(elems) < 2 {
		return "", fmt.Errorf("%s: %w", importPath, module.ErrNotFound)
	}
	base := r.ModuleHost + "/" + elems[0] + "/" + elems[1]
	var candidates []string
	if len(elems) >= 3 && majorSuffix.MatchString(elems[2]) {
		candidates = append(candidates, base+"/"+elems[2])
	}
	candidates = append(candidates, base)
	for _, c := range candidates {
		if _, err := r.CheckModulePath(c); err != nil {
			continue
		}
		_, err := r.Versions(ctx, c)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, module.ErrNotFound) || errors.Is(err, ErrQuarantined) {
			return "", err
		}
	}
	return "", fmt.Errorf("%s: %w", importPath, module.ErrNotFound)
}

var majorSuffix = regexp.MustCompile(`^v([2-9]|[1-9][0-9]+)$`)

// IsLocalHost reports whether a module host only exists on developer
// machines (gopherdex.localhost, *.test, …). The public Go module mirror
// can't reach such hosts, so installs need GOPROXY set explicitly.
func IsLocalHost(host string) bool {
	for _, suffix := range []string{".localhost", ".test", ".local", ".internal", ".invalid", ".example", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// VersionDetail is everything a project page shows about one version.
type VersionDetail struct {
	Version     string
	PublishedAt time.Time
	PublishedBy string // username, or "" if the account is gone
	H1          string
	GoModH1     string
	SHA256      string
	Size        int64
	Repository  string
	Commit      string
	Ref         string
	Yanked      bool
	YankReason  string
	YankedAt    time.Time
	// Provenance is set when a trusted publisher uploaded the version.
	Provenance *Provenance
}

// ModuleDetail describes a published module and its versions.
type ModuleDetail struct {
	Path        string
	Namespace   string
	CreatedAt   time.Time
	Deprecation string          // set on the website; see also // Deprecated: in go.mod
	Successor   string          // module to use instead, if the owners named one
	Versions    []VersionDetail // newest first by semver
}

// Module returns a published module with all its versions.
func (r *Registry) Module(ctx context.Context, modPath string) (*ModuleDetail, error) {
	m := &ModuleDetail{Path: modPath}
	var created int64
	var id int64
	var quarantined sql.NullInt64
	err := r.DB.QueryRowContext(ctx, `SELECT id, namespace, created_at, deprecation, successor, quarantined_at FROM modules WHERE path = ?`, modPath).
		Scan(&id, &m.Namespace, &created, &m.Deprecation, &m.Successor, &quarantined)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("module %s: %w", modPath, module.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load module %s: %w", modPath, err)
	}
	if quarantined.Valid {
		return nil, fmt.Errorf("%s: %w", modPath, ErrQuarantined)
	}
	m.CreatedAt = time.Unix(created, 0).UTC()

	rows, err := r.DB.QueryContext(ctx, `SELECT v.version, v.published_at, COALESCE(u.username, ''), v.h1, v.go_mod_h1, v.zip_sha256,
			v.zip_size, v.repository, v.commit_hash, v.ref, v.yanked_at, v.yank_reason, v.provenance
		FROM versions v LEFT JOIN users u ON u.id = v.published_by WHERE v.module_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("load versions of %s: %w", modPath, err)
	}
	defer rows.Close()
	for rows.Next() {
		var v VersionDetail
		var published int64
		var yanked sql.NullInt64
		var provenance string
		if err := rows.Scan(&v.Version, &published, &v.PublishedBy, &v.H1, &v.GoModH1, &v.SHA256, &v.Size, &v.Repository, &v.Commit, &v.Ref,
			&yanked, &v.YankReason, &provenance); err != nil {
			return nil, fmt.Errorf("load versions of %s: %w", modPath, err)
		}
		v.PublishedAt = time.Unix(published, 0).UTC()
		v.Provenance = ParseProvenance(provenance)
		if yanked.Valid {
			v.Yanked, v.YankedAt = true, time.Unix(yanked.Int64, 0).UTC()
		}
		m.Versions = append(m.Versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(m.Versions) == 0 {
		return nil, fmt.Errorf("module %s has no versions: %w", modPath, module.ErrNotFound)
	}
	slices.SortFunc(m.Versions, func(a, b VersionDetail) int { return module.Compare(b.Version, a.Version) })
	return m, nil
}

// MajorVersions lists the published major versions of a module, e.g.
// …/retry, …/retry/v2, …/retry/v3, lowest first.
func (r *Registry) MajorVersions(ctx context.Context, modPath string) ([]string, error) {
	base, _, _ := xmodule.SplitPathVersion(modPath)
	// A range on the path index, not LIKE: SQLite can't use an index for
	// LIKE here, and scanning every module made this most of a project
	// page's cost at 100,000 modules.
	rows, err := r.DB.QueryContext(ctx, `SELECT m.path FROM modules m
		WHERE (m.path = ? OR (m.path >= ? AND m.path < ?)) AND EXISTS (SELECT 1 FROM versions v WHERE v.module_id = m.id)`,
		base, base+"/v", base+"/w")
	if err != nil {
		return nil, fmt.Errorf("list major versions of %s: %w", base, err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if _, major, _ := xmodule.SplitPathVersion(p); p == base || strings.TrimPrefix(p, base) == major {
			paths = append(paths, p)
		}
	}
	slices.SortFunc(paths, func(a, b string) int {
		_, ma, _ := xmodule.SplitPathVersion(a)
		_, mb, _ := xmodule.SplitPathVersion(b)
		return module.Compare(strings.TrimPrefix(ma, "/")+".0.0", strings.TrimPrefix(mb, "/")+".0.0")
	})
	return paths, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// NamespaceExists reports whether a user or organization owns name.
func (r *Registry) NamespaceExists(ctx context.Context, name string) (bool, error) {
	var n int
	if err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM namespaces WHERE name = ?`, name).Scan(&n); err != nil {
		return false, fmt.Errorf("look up namespace %s: %w", name, err)
	}
	return n > 0, nil
}

// Stats counts what the registry holds.
type Stats struct {
	Modules    int
	Releases   int
	Publishers int
}

// Stats returns registry-wide counts for the landing page.
func (r *Registry) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := r.DB.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(DISTINCT module_id) FROM versions),
			(SELECT COUNT(*) FROM versions),
			(SELECT COUNT(DISTINCT published_by) FROM versions WHERE published_by IS NOT NULL)`).
		Scan(&s.Modules, &s.Releases, &s.Publishers)
	if err != nil {
		return Stats{}, fmt.Errorf("registry stats: %w", err)
	}
	return s, nil
}

// Listing is a module in a list: its newest release and summary.
type Listing struct {
	Path        string
	Namespace   string
	Version     string
	Synopsis    string
	PublishedAt time.Time // of Version
	CreatedAt   time.Time // of the module
}

// listingQuery selects each module with its most recently published
// version. Callers order it by an indexed column and stop at a limit, so
// SQLite walks the newest rows and checks each is its module's latest,
// instead of finding every module's latest release first.
const listingQuery = `SELECT m.path, m.namespace, v.version, v.synopsis, v.published_at, m.created_at
	FROM versions v JOIN modules m ON m.id = v.module_id
	WHERE v.yanked_at IS NULL AND ` + isLatest

// RecentlyUpdated lists modules by their latest release, newest first.
func (r *Registry) RecentlyUpdated(ctx context.Context, limit, offset int) ([]Listing, error) {
	return r.listings(ctx, listingQuery+` ORDER BY v.published_at DESC, v.id DESC LIMIT ? OFFSET ?`, limit, offset)
}

// NewModules lists modules by when they were first published, newest first.
func (r *Registry) NewModules(ctx context.Context, limit int) ([]Listing, error) {
	return r.listings(ctx, listingQuery+` ORDER BY m.created_at DESC, m.id DESC LIMIT ?`, limit)
}

func (r *Registry) listings(ctx context.Context, query string, args ...any) ([]Listing, error) {
	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list modules: %w", err)
	}
	defer rows.Close()
	out := []Listing{}
	for rows.Next() {
		var l Listing
		var published, created int64
		if err := rows.Scan(&l.Path, &l.Namespace, &l.Version, &l.Synopsis, &published, &created); err != nil {
			return nil, fmt.Errorf("list modules: %w", err)
		}
		l.PublishedAt, l.CreatedAt = time.Unix(published, 0).UTC(), time.Unix(created, 0).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}
