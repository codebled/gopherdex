package registry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	xmodule "golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/codebled/gopherdex/internal/accounts"
	"github.com/codebled/gopherdex/internal/blob"
	"github.com/codebled/gopherdex/internal/database"
	"github.com/codebled/gopherdex/internal/mail"
	"github.com/codebled/gopherdex/internal/module"
)

const host = "gopherdex.test"

type nopMailer struct{}

func (nopMailer) Send(context.Context, mail.Message) error { return nil }

type fixture struct {
	reg   *Registry
	accts *accounts.Service
	alice *accounts.User
	token *accounts.Token
	dir   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blob.OpenFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })

	accts := &accounts.Service{DB: db, Mailer: nopMailer{}, BaseURL: "http://" + host}
	f := &fixture{
		reg:   &Registry{DB: db, Blobs: blobs, ModuleHost: host, MaxZipSize: 1 << 20},
		accts: accts,
		dir:   dir,
	}
	f.alice, f.token = f.user(t, "alice")
	return f
}

// user registers a verified account with an API token.
func (f *fixture) user(t *testing.T, name string) (*accounts.User, *accounts.Token) {
	t.Helper()
	ctx := context.Background()
	u, err := f.accts.Register(ctx, name, name+"@example.com", "correct horse battery", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.accts.DB.ExecContext(ctx, `UPDATE users SET email_verified_at = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	u.EmailVerified = true
	_, tok, err := f.accts.CreateToken(ctx, u, "test", 0, "", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}
	return u, tok
}

// zipModule writes files into a directory and builds a module zip from it
// with golang.org/x/mod/zip, the same code the go command uses.
func (f *fixture) zipModule(t *testing.T, modPath, version string, files map[string]string) string {
	t.Helper()
	src := t.TempDir()
	for name, body := range files {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := os.CreateTemp(f.dir, "*.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := modzip.CreateFromDir(out, xmodule.Version{Path: modPath, Version: version}, src); err != nil {
		t.Fatal(err)
	}
	return out.Name()
}

func (f *fixture) upload(modPath, version, zipFile string) Upload {
	return Upload{User: f.alice, Token: f.token, Module: modPath, Version: version, ZipFile: zipFile,
		Repository: "https://github.com/alice/retry", Commit: "0123456789abcdef0123456789abcdef01234567", Ref: "refs/tags/" + version}
}

func retryFiles(modPath, extra string) map[string]string {
	return map[string]string{
		"go.mod":    "module " + modPath + "\n\ngo 1.22\n",
		"retry.go":  "// Package retry retries operations with backoff.\npackage retry\n\n// Do runs fn until it succeeds." + extra + "\nfunc Do(fn func() error) error { return fn() }\n",
		"README.md": "# retry\n",
	}
}

func TestPublishAndServe(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"

	zip1 := f.zipModule(t, mod, "v1.0.0", retryFiles(mod, ""))
	pub, err := f.reg.Publish(ctx, f.upload(mod, "v1.0.0", zip1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pub.H1, "h1:") || !strings.HasPrefix(pub.GoModH1, "h1:") || pub.AlreadyPublished || pub.Size == 0 {
		t.Fatalf("Published = %+v", pub)
	}

	// Retrying the identical upload is harmless.
	again, err := f.reg.Publish(ctx, f.upload(mod, "v1.0.0", zip1))
	if err != nil || !again.AlreadyPublished || again.H1 != pub.H1 {
		t.Fatalf("identical re-upload = %+v, %v", again, err)
	}
	// Different content for the same version is refused.
	changed := f.zipModule(t, mod, "v1.0.0", retryFiles(mod, " Changed."))
	if _, err := f.reg.Publish(ctx, f.upload(mod, "v1.0.0", changed)); !isReject(err, http.StatusConflict, "version_exists") {
		t.Fatalf("changed re-upload: %v", err)
	}

	zip2 := f.zipModule(t, mod, "v1.1.0", retryFiles(mod, " Now faster."))
	if _, err := f.reg.Publish(ctx, f.upload(mod, "v1.1.0", zip2)); err != nil {
		t.Fatal(err)
	}

	// Serving side.
	versions, err := f.reg.Versions(ctx, mod)
	if err != nil || !slices.Equal(versions, []string{"v1.0.0", "v1.1.0"}) {
		t.Fatalf("Versions = %v, %v", versions, err)
	}
	info, err := f.reg.Latest(ctx, mod)
	if err != nil || info.Version != "v1.1.0" || info.Origin == nil || info.Origin.Hash == "" || info.Origin.URL != "https://github.com/alice/retry" {
		t.Fatalf("Latest = %+v, %v", info, err)
	}
	goMod, err := f.reg.GoMod(ctx, mod, "v1.0.0")
	if err != nil || !strings.HasPrefix(string(goMod), "module "+mod) {
		t.Fatalf("GoMod = %q, %v", goMod, err)
	}
	wt, err := f.reg.Zip(ctx, mod, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	wt.WriteTo(&buf)
	want, _ := os.ReadFile(zip1)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatal("served zip differs from the upload")
	}
	fsys, closer, err := f.reg.VersionFS(ctx, mod, "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	src, err := fs.ReadFile(fsys, "retry.go")
	closer.Close()
	if err != nil || !strings.Contains(string(src), "Now faster.") {
		t.Fatalf("VersionFS retry.go = %q, %v", src, err)
	}
	mods, err := f.reg.Modules(ctx)
	if err != nil || !slices.Equal(mods, []string{mod}) {
		t.Fatalf("Modules = %v, %v", mods, err)
	}
	summary, err := f.reg.NamespaceModules(ctx, "alice")
	if err != nil || len(summary) != 1 || summary[0].Latest != "v1.1.0" || summary[0].Versions != 2 {
		t.Fatalf("NamespaceModules = %+v, %v", summary, err)
	}
	if _, err := f.reg.Versions(ctx, host+"/alice/missing"); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("missing module: %v", err)
	}

	stats, err := f.reg.Stats(ctx)
	if err != nil || stats != (Stats{Modules: 1, Releases: 2, Publishers: 1}) {
		t.Fatalf("Stats = %+v, %v", stats, err)
	}
	recent, err := f.reg.RecentlyUpdated(ctx, 10, 0)
	if err != nil || len(recent) != 1 || recent[0].Version != "v1.1.0" || recent[0].Synopsis != "Package retry retries operations with backoff." || recent[0].Namespace != "alice" {
		t.Fatalf("RecentlyUpdated = %+v, %v", recent, err)
	}
	fresh, err := f.reg.NewModules(ctx, 10)
	if err != nil || len(fresh) != 1 || fresh[0].Path != mod {
		t.Fatalf("NewModules = %+v, %v", fresh, err)
	}
}

func TestPublishRejects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	good := f.zipModule(t, mod, "v1.0.0", retryFiles(mod, ""))
	bob, bobToken := f.user(t, "bob")

	tests := []struct {
		name   string
		upload func() Upload
		status int
		code   string
	}{
		{"other host", func() Upload { return f.upload("github.com/alice/retry", "v1.0.0", good) }, 400, "wrong_host"},
		{"missing name", func() Upload { return f.upload(host+"/alice", "v1.0.0", good) }, 400, "invalid_module_path"},
		{"nested path", func() Upload { return f.upload(host+"/alice/a/b", "v1.0.0", good) }, 400, "invalid_module_path"},
		{"upper-case name", func() Upload { return f.upload(host+"/alice/Retry", "v1.0.0", good) }, 400, "invalid_module_name"},
		{"someone else's namespace", func() Upload {
			u := f.upload(mod, "v1.0.0", good)
			u.User, u.Token = bob, bobToken
			return u
		}, 403, "forbidden_namespace"},
		{"short version", func() Upload { return f.upload(mod, "v1.0", good) }, 400, "invalid_version"},
		{"pseudo-version", func() Upload { return f.upload(mod, "v0.0.0-20260101000000-abcdefabcdef", good) }, 400, "invalid_version"},
		{"v2 without suffix", func() Upload { return f.upload(mod, "v2.0.0", good) }, 400, "major_version_mismatch"},
		{"zip for another version", func() Upload { return f.upload(mod, "v1.0.1", good) }, 422, "invalid_zip"},
		{"bad repository", func() Upload {
			u := f.upload(mod, "v1.0.0", good)
			u.Repository = "https://user:pass@github.com/alice/retry"
			return u
		}, 400, "invalid_repository"},
		{"bad commit", func() Upload {
			u := f.upload(mod, "v1.0.0", good)
			u.Commit = "not-a-hash"
			return u
		}, 400, "invalid_commit"},
		{"no go.mod", func() Upload {
			return f.upload(mod, "v1.0.0", f.zipModule(t, mod, "v1.0.0", map[string]string{"retry.go": "package retry\n"}))
		}, 422, "missing_go_mod"},
		{"go.mod for another path", func() Upload {
			return f.upload(mod, "v1.0.0", f.zipModule(t, mod, "v1.0.0", retryFiles(host+"/alice/other", "")))
		}, 422, "go_mod_path_mismatch"},
		{"too large", func() Upload {
			return f.upload(mod, "v1.0.0", f.zipModule(t, mod, "v1.0.0", map[string]string{
				"go.mod": "module " + mod + "\n", "big.txt": randomText(2 << 20),
			}))
		}, 413, "zip_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.reg.Publish(ctx, tt.upload())
			if !isReject(err, tt.status, tt.code) {
				t.Fatalf("got %v, want %d %s", err, tt.status, tt.code)
			}
		})
	}

	// v2 with the right suffix is fine.
	v2 := host + "/alice/retry/v2"
	if _, err := f.reg.Publish(ctx, f.upload(v2, "v2.0.0", f.zipModule(t, v2, "v2.0.0", retryFiles(v2, "")))); err != nil {
		t.Fatalf("v2 publish: %v", err)
	}
	if _, err := f.reg.Versions(ctx, mod); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("rejected uploads must not create versions: %v", err)
	}
}

func TestMulti(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	if _, err := f.reg.Publish(ctx, f.upload(mod, "v1.0.0", f.zipModule(t, mod, "v1.0.0", retryFiles(mod, "")))); err != nil {
		t.Fatal(err)
	}
	m := Multi{emptySource{}, f.reg}
	if info, err := m.Latest(ctx, mod); err != nil || info.Version != "v1.0.0" {
		t.Fatalf("Multi.Latest = %+v, %v", info, err)
	}
	if _, err := m.GoMod(ctx, host+"/alice/nope", "v1.0.0"); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("Multi missing module: %v", err)
	}
}

type emptySource struct{}

func (emptySource) Modules(context.Context) ([]string, error) { return nil, nil }
func (emptySource) Versions(context.Context, string) ([]string, error) {
	return nil, module.ErrNotFound
}
func (emptySource) Info(context.Context, string, string) (module.Info, error) {
	return module.Info{}, module.ErrNotFound
}
func (emptySource) Latest(context.Context, string) (module.Info, error) {
	return module.Info{}, module.ErrNotFound
}
func (emptySource) GoMod(context.Context, string, string) ([]byte, error) {
	return nil, module.ErrNotFound
}
func (emptySource) Zip(context.Context, string, string) (io.WriterTo, error) {
	return nil, module.ErrNotFound
}
func (emptySource) VersionFS(context.Context, string, string) (fs.FS, io.Closer, error) {
	return nil, nil, module.ErrNotFound
}

func isReject(err error, status int, code string) bool {
	var re *Error
	return errors.As(err, &re) && re.Status == status && re.Code == code
}

// randomText is incompressible enough to exceed the zip size limit.
func randomText(n int) string {
	var b strings.Builder
	x := uint64(time.Now().UnixNano())
	for b.Len() < n {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b.WriteByte("abcdefghijklmnopqrstuvwxyz0123456789"[x%36])
	}
	return b.String()
}

// setOrgMember adds someone to an organization and accepts the invitation
// as them, for tests about what members can do. TestInvitations covers the
// invitation itself.
func (f *fixture) setOrgMember(ctx context.Context, u *accounts.User, org, username, role string, c accounts.Client) error {
	if err := f.reg.SetOrgMember(ctx, u, org, username, role, c); err != nil {
		return err
	}
	return f.acceptAs(ctx, username, InviteOrg, org)
}

// setCollaborator does the same for a role on a module.
func (f *fixture) setCollaborator(ctx context.Context, u *accounts.User, modPath, username, role string, c accounts.Client) error {
	if err := f.reg.SetCollaborator(ctx, u, modPath, username, role, c); err != nil {
		return err
	}
	return f.acceptAs(ctx, username, InviteModule, modPath)
}

func (f *fixture) acceptAs(ctx context.Context, username, kind, name string) error {
	var id int64
	if err := f.reg.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, accounts.NormalizeUsername(strings.TrimPrefix(username, "@"))).Scan(&id); err != nil {
		return err
	}
	u, err := f.accts.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if err := f.reg.AcceptInvitation(ctx, u, kind, name, client); err != nil && !isReject(err, http.StatusNotFound, "no_invitation") {
		return err // already accepted is fine: a role change
	}
	return nil
}
