package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/blob"
	"github.com/parthiban-sivakumar/gopherdex/internal/database"
	"github.com/parthiban-sivakumar/gopherdex/internal/discovery"
	"github.com/parthiban-sivakumar/gopherdex/internal/goproxy"
	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
	"github.com/parthiban-sivakumar/gopherdex/internal/server"
)

const moduleHost = "gopherdex.test"

type nopMailer struct{}

func (nopMailer) Send(context.Context, mail.Message) error { return nil }

// startRegistry runs a real Gopherdex server with a verified user "alice"
// and returns its URL and alice's API token.
func startRegistry(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	tr := startRegistryWith(t, nil)
	return tr.srv, tr.token
}

type testRegistry struct {
	srv   *httptest.Server
	token string
	reg   *registry.Registry
	alice *accounts.User
}

// startRegistryWith is startRegistry with a hook to adjust the server's
// configuration.
func startRegistryWith(t *testing.T, configure func(*server.Config)) *testRegistry {
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

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	accts := &accounts.Service{DB: db, Mailer: nopMailer{}, BaseURL: "http://" + moduleHost, Log: log}
	reg := &registry.Registry{DB: db, Blobs: blobs, ModuleHost: moduleHost, Log: log}
	hosted := registry.Multi{reg}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	cfg := server.Config{
		Discovery: &discovery.Service{Local: hosted, ProxyPrefix: "/api/proxy", Log: log},
		Accounts:  accts, Registry: reg, Proxy: goproxy.NewHandler(hosted, log), Log: log,
		SiteURL: srv.URL, ModuleHost: moduleHost,
	}
	if configure != nil {
		configure(&cfg)
	}
	h, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = h

	u, err := accts.Register(ctx, "alice", "alice@example.com", "correct horse battery", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}
	db.ExecContext(ctx, `UPDATE users SET email_verified_at = 1 WHERE id = ?`, u.ID)
	u.EmailVerified = true
	token, _, err := accts.CreateToken(ctx, u, "cli test", 0, "", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}
	return &testRegistry{srv: srv, token: token, reg: reg, alice: u}
}

type runner struct {
	t      *testing.T
	env    map[string]string
	dir    string
	client *http.Client
}

func (r *runner) run(stdin string, args ...string) (int, string, string) {
	r.t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(args, Env{
		Stdin: strings.NewReader(stdin), Stdout: &stdout, Stderr: &stderr,
		Getenv: func(k string) string { return r.env[k] },
		Dir:    r.dir, HTTP: r.client,
	})
	return code, stdout.String(), stderr.String()
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("output does not contain %q:\n%s", want, got)
	}
}

// gitRepo creates a repository with a module and commits it.
func gitRepo(t *testing.T, modPath string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":   "module " + modPath + "\n\ngo 1.22\n",
		"retry.go": "// Package retry retries operations.\npackage retry\n\n// Do runs fn once.\nfunc Do(fn func() error) error { return fn() }\n",
		"notes.md": "draft\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first release")
	gitRun(t, dir, "remote", "add", "origin", "git@github.com:alice/retry.git")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestLoginWhoamiPublish(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv, token := startRegistry(t)
	mod := moduleHost + "/alice/retry"
	repo := gitRepo(t, mod)
	configFile := filepath.Join(t.TempDir(), "credentials.json")
	r := &runner{t: t, env: map[string]string{"GOPHERDEX_CONFIG": configFile}, dir: repo, client: srv.Client()}

	code, _, stderr := r.run("", "whoami")
	if code != 1 {
		t.Fatalf("whoami before login: exit %d", code)
	}
	mustContain(t, stderr, "no registry chosen")

	code, _, stderr = r.run("gdx_not-a-real-token\n", "login", "--registry", srv.URL)
	if code != 1 {
		t.Fatalf("login with bad token: exit %d", code)
	}
	mustContain(t, stderr, "rejected that token")

	code, stdout, stderr := r.run(token+"\n", "login", "--registry", srv.URL+"/")
	if code != 0 {
		t.Fatalf("login: exit %d\n%s", code, stderr)
	}
	mustContain(t, stdout, "Signed in to "+srv.URL+" as @alice")
	if st, err := os.Stat(configFile); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}

	_, stdout, _ = r.run("", "whoami")
	mustContain(t, stdout, "@alice on "+srv.URL)
	mustContain(t, stdout, "namespace  "+moduleHost+"/alice/")

	// No tag on HEAD yet.
	code, _, stderr = r.run("", "publish")
	if code != 1 {
		t.Fatalf("publish without tag: exit %d", code)
	}
	mustContain(t, stderr, "no v* release tag")

	gitRun(t, repo, "tag", "v0.1.0")
	// A file changed after tagging must not end up in the release.
	os.WriteFile(filepath.Join(repo, "notes.md"), []byte("uncommitted edit\n"), 0o644)

	code, stdout, stderr = r.run("", "publish", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d\n%s", code, stderr)
	}
	mustContain(t, stdout, "Checked "+mod+"@v0.1.0")
	mustContain(t, stdout, "Dry run: nothing uploaded")
	mustContain(t, stdout, "source   https://github.com/alice/retry")

	code, stdout, stderr = r.run("", "publish")
	if code != 0 {
		t.Fatalf("publish: exit %d\n%s", code, stderr)
	}
	mustContain(t, stdout, "Published "+mod+"@v0.1.0")
	mustContain(t, stdout, "install  GOPROXY="+srv.URL+"/api/proxy GONOSUMDB="+moduleHost+" go get "+mod+"@v0.1.0")

	code, stdout, _ = r.run("", "publish", "v0.1.0")
	if code != 0 {
		t.Fatalf("republish: exit %d", code)
	}
	mustContain(t, stdout, "already published with identical content")

	// The go command can now see it.
	resp, err := srv.Client().Get(srv.URL + "/api/proxy/" + mod + "/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "v0.1.0\n" {
		t.Fatalf("@v/list = %q", body)
	}
	resp, err = srv.Client().Get(srv.URL + "/api/proxy/" + mod + "/@v/v0.1.0.zip")
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	notes, err := fs.ReadFile(zr, mod+"@v0.1.0/notes.md")
	if err != nil || string(notes) != "draft\n" {
		t.Fatalf("published notes.md = %q, %v; want the tagged content, not the working-tree edit", notes, err)
	}

	// Major version rule.
	gitRun(t, repo, "tag", "v2.0.0")
	code, _, stderr = r.run("", "publish", "v2.0.0")
	if code != 1 {
		t.Fatalf("v2 without suffix: exit %d", code)
	}
	mustContain(t, stderr, "module "+mod+"/v2")

	// Someone else's namespace.
	other := gitRepo(t, moduleHost+"/bob/retry")
	gitRun(t, other, "tag", "v1.0.0")
	r.dir = other
	code, _, stderr = r.run("", "publish")
	if code != 1 {
		t.Fatalf("publish to bob: exit %d", code)
	}
	mustContain(t, stderr, "@alice can't create modules in "+moduleHost+"/bob/")

	// Yank and restore a release.
	r.dir = repo
	code, stdout, stderr = r.run("", "yank", "--reason", "Broken build", mod+"@v0.1.0")
	if code != 0 {
		t.Fatalf("yank: exit %d\n%s", code, stderr)
	}
	mustContain(t, stdout, "Yanked "+mod+"@v0.1.0")
	resp, err = srv.Client().Get(srv.URL + "/api/proxy/" + mod + "/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "" {
		t.Fatalf("@v/list after yank = %q, want empty", body)
	}
	code, stdout, _ = r.run("", "unyank", mod+"@v0.1.0")
	if code != 0 {
		t.Fatalf("unyank: exit %d", code)
	}
	mustContain(t, stdout, "Restored")
	code, _, stderr = r.run("", "yank", mod)
	if code != 1 {
		t.Fatalf("yank without version: exit %d", code)
	}
	mustContain(t, stderr, "MODULE@VERSION")

	code, stdout, _ = r.run("", "logout")
	if code != 0 {
		t.Fatalf("logout: exit %d", code)
	}
	mustContain(t, stdout, "Removed the saved token")
}

func TestNormalizeRegistry(t *testing.T) {
	for in, want := range map[string]string{
		"https://gopherdex.dev/":  "https://gopherdex.dev",
		"http://localhost:8080":   "http://localhost:8080",
		"http://127.0.0.1:9000/x": "http://127.0.0.1:9000/x",
	} {
		if got, err := normalizeRegistry(in); err != nil || got != want {
			t.Errorf("normalizeRegistry(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://gopherdex.dev", "gopherdex.dev", "https://user:pw@gopherdex.dev", "ftp://x.dev"} {
		if _, err := normalizeRegistry(bad); err == nil {
			t.Errorf("normalizeRegistry(%q) accepted", bad)
		}
	}
}
