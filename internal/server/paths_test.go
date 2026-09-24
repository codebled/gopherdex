package server

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xmodule "golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/codebled/gopherdex/internal/accounts"
	"github.com/codebled/gopherdex/internal/registry"
)

// publish puts a minimal module version into the registry as alice.
func (e *testEnv) publish(t *testing.T, modPath, version string) {
	t.Helper()
	ctx := context.Background()
	u, err := e.accts.Register(ctx, "alice", "alice@example.com", "correct horse battery", accounts.Client{})
	if err != nil && !errors.Is(err, accounts.ErrUsernameTaken) {
		t.Fatal(err)
	}
	e.accts.DB.ExecContext(ctx, `UPDATE users SET email_verified_at = 1 WHERE username = 'alice'`)
	if u == nil {
		var id int64
		e.accts.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'alice'`).Scan(&id)
		u = &accounts.User{ID: id, Username: "alice"}
	}
	u.EmailVerified = true
	_, tok, err := e.accts.CreateToken(ctx, u, "test", 0, "", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module "+modPath+"\n"), 0o644)
	os.WriteFile(filepath.Join(src, "lib.go"), []byte("// Package lib is a test.\npackage lib\n"), 0o644)
	zipFile := filepath.Join(t.TempDir(), "m.zip")
	f, _ := os.Create(zipFile)
	if err := modzip.CreateFromDir(f, xmodule.Version{Path: modPath, Version: version}, src); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := e.reg.Publish(ctx, registry.Upload{User: u, Token: tok, Module: modPath, Version: version, ZipFile: zipFile}); err != nil {
		t.Fatal(err)
	}
}

type goImport struct{ Prefix, VCS, RepoRoot string }

// parseGoImports reads go-import tags the way cmd/go does: a lenient XML
// scan of <head> that stops at <body>.
func parseGoImports(t *testing.T, r io.Reader) []goImport {
	t.Helper()
	d := xml.NewDecoder(r)
	d.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	d.Strict = false
	var out []goImport
	for {
		tok, err := d.RawToken()
		if err != nil {
			return out
		}
		if e, ok := tok.(xml.StartElement); ok && strings.EqualFold(e.Name.Local, "body") {
			return out
		}
		e, ok := tok.(xml.StartElement)
		if !ok || !strings.EqualFold(e.Name.Local, "meta") {
			continue
		}
		var name, content string
		for _, a := range e.Attr {
			switch strings.ToLower(a.Name.Local) {
			case "name":
				name = a.Value
			case "content":
				content = a.Value
			}
		}
		if name != "go-import" {
			continue
		}
		f := strings.Fields(content)
		if len(f) != 3 {
			t.Fatalf("go-import content %q must have 3 fields", content)
		}
		out = append(out, goImport{f[0], f[1], f[2]})
	}
}

func TestGoGet(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	env.publish(t, "gopherdex.test/alice/retry/v2", "v2.0.0")
	client := env.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	tests := []struct{ path, root string }{
		{"/alice/retry", "gopherdex.test/alice/retry"},
		{"/alice/retry/internal/backoff", "gopherdex.test/alice/retry"},
		{"/alice/retry/v2", "gopherdex.test/alice/retry/v2"},
		{"/alice/retry/v2/sub", "gopherdex.test/alice/retry/v2"},
		{"/alice/retry/v3", "gopherdex.test/alice/retry"}, // no v3 module: a package path inside v1
	}
	for _, tt := range tests {
		resp, err := client.Get(env.srv.URL + tt.path + "?go-get=1")
		if err != nil {
			t.Fatal(err)
		}
		imports := parseGoImports(t, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", tt.path, resp.StatusCode)
		}
		want := goImport{tt.root, "mod", "http://gopherdex.test/api/proxy"}
		if len(imports) != 1 || imports[0] != want {
			t.Errorf("%s?go-get=1: go-import = %+v, want %+v", tt.path, imports, want)
		}
	}

	for _, path := range []string{"/alice/nope?go-get=1", "/bob/retry?go-get=1", "/alice?go-get=1", "/nope", "/account/tokens", "/alice/retry@v9.9.9", "/alice/retry@latest"} {
		resp, err := client.Get(env.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, resp.StatusCode)
		}
	}

	// People following a package import path land on its docs.
	resp, err := client.Get(env.srv.URL + "/alice/retry/internal/backoff")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "/alice/retry?tab=docs#pkg-internal-backoff" {
		t.Fatalf("package path: %d Location %q", resp.StatusCode, loc)
	}

	// Existing routes still win over the module path pattern.
	for path, status := range map[string]int{"/static/app.css": 200, "/login": 200, "/healthz": 200} {
		resp, err := client.Get(env.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != status {
			t.Errorf("GET %s: status %d, want %d", path, resp.StatusCode, status)
		}
	}
}

func TestInstallCommand(t *testing.T) {
	public := &server{moduleHost: "gopherdex.dev", siteURL: "https://gopherdex.dev", zeroConfig: true}
	if got := public.installCommand("gopherdex.dev/alice/retry", "v1.0.0"); got != "go get gopherdex.dev/alice/retry@v1.0.0" {
		t.Errorf("public host: %q", got)
	}
	if got := public.installCommand("example.com/hello", "latest"); !strings.HasPrefix(got, "GOPROXY=https://gopherdex.dev/api/proxy GONOSUMDB=example.com ") {
		t.Errorf("fixture module on public registry: %q", got)
	}
	local := &server{moduleHost: "gopherdex.localhost", siteURL: "http://localhost:8080"}
	if got := local.installCommand("gopherdex.localhost/alice/retry", "latest"); got != "GOPROXY=http://localhost:8080/api/proxy GONOSUMDB=gopherdex.localhost go get gopherdex.localhost/alice/retry@latest" {
		t.Errorf("local host: %q", got)
	}
}

func TestClientIPBehindProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.2:5000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")

	if ip := (&server{}).clientOf(r).IP; ip != "10.0.0.2" {
		t.Errorf("untrusted: IP %q, want the socket address", ip)
	}
	if ip := (&server{trustProxy: true}).clientOf(r).IP; ip != "203.0.113.9" {
		t.Errorf("trusted proxy: IP %q, want the entry the proxy appended", ip)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if ip := (&server{trustProxy: true}).clientOf(r).IP; ip != "10.0.0.2" {
		t.Errorf("garbage header: IP %q", ip)
	}
}

func TestProjectPages(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	env.publish(t, "gopherdex.test/alice/retry", "v1.1.0")
	env.publish(t, "gopherdex.test/alice/retry/v2", "v2.0.0")
	client := env.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := client.Get(env.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	tests := []struct {
		path     string
		status   int
		contains []string
	}{
		{"/alice/retry", 200, []string{"<h1", "v1.1.0", "Latest version", "Project description", "GOPROXY=http://gopherdex.test/api/proxy", "Major versions"}},
		{"/alice/retry@v1.0.0", 200, []string{"Newer version available: v1.1.0", "go get gopherdex.test/alice/retry@v1.0.0", `href="/alice/retry@v1.0.0?tab=docs"`}},
		{"/alice/retry?tab=docs", 200, []string{"Package lib is a test.", `id="pkg"`}},
		{"/alice/retry?tab=versions", 200, []string{`href="/alice/retry@v1.0.0"`, "@alice"}},
		{"/alice/retry?tab=files", 200, []string{"retry@v1.1.0.zip", "gopherdex.test/alice/retry v1.1.0 h1:", "/api/proxy/gopherdex.test/alice/retry/@v/v1.1.0.zip"}},
		{"/alice/retry/v2", 200, []string{`<span class="major">/v2</span>`, "v2.0.0"}},
		{"/alice", 200, []string{"@alice", `href="/alice/retry"`, `href="/alice/retry/v2"`}},
		{"/nobody", 404, []string{"nothing at this address"}},
	}
	for _, tt := range tests {
		resp, body := get(tt.path)
		if resp.StatusCode != tt.status {
			t.Errorf("GET %s: status %d, want %d", tt.path, resp.StatusCode, tt.status)
			continue
		}
		for _, want := range tt.contains {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: body is missing %q", tt.path, want)
			}
		}
	}

	// Old library-view links and search results for published modules redirect.
	resp, _ := get("/?m=gopherdex.test%2Falice%2Fretry&v=v1.0.0")
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "/alice/retry@v1.0.0" {
		t.Errorf("/?m= redirect: %d %q", resp.StatusCode, loc)
	}
	_, body := get("/")
	for _, want := range []string{"Just updated", "New modules", `href="/alice/retry"`, "<dd>2</dd>", "<dd>3</dd>", "<dd>1</dd>"} {
		if !strings.Contains(body, want) {
			t.Errorf("landing page is missing %q", want)
		}
	}
	if strings.Contains(body, "How it works") || strings.Contains(body, "GONOSUMDB") {
		t.Error("the landing page should showcase modules, not explain the proxy")
	}
	_, body = get("/search")
	if !strings.Contains(body, "2 modules") || !strings.Contains(body, "Package lib is a test.") {
		t.Errorf("browse page is missing modules or summaries")
	}
	_, body = get("/search?q=retry&sort=new")
	if !strings.Contains(body, `href="/alice/retry"`) || !strings.Contains(body, "result") {
		t.Errorf("search page is missing the module")
	}
	resp, _ = get("/modules")
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("/modules: status %d, want a redirect to /search", resp.StatusCode)
	}
}

func TestDownloadCounts(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	for range 3 {
		resp, err := env.srv.Client().Get(env.srv.URL + "/api/proxy/gopherdex.test/alice/retry/@v/v1.0.0.zip")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// go.mod and .info fetches aren't downloads.
	resp, _ := env.srv.Client().Get(env.srv.URL + "/api/proxy/gopherdex.test/alice/retry/@v/v1.0.0.mod")
	resp.Body.Close()
	if err := env.downloads.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	get := func(path string) string {
		t.Helper()
		resp, err := env.srv.Client().Get(env.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	body := get("/alice/retry")
	if !strings.Contains(body, "<dt>Last day</dt><dd>3</dd>") || !strings.Contains(body, `class="dl-bar"`) {
		t.Error("project page should show 3 downloads today and a chart")
	}
	if !strings.Contains(get("/alice/retry?tab=versions"), `<td class="num">3</td>`) {
		t.Error("release history should show per-version downloads")
	}
	if body := get("/"); !strings.Contains(body, "Most downloaded") || !strings.Contains(body, "3 downloads this month") {
		t.Error("home page should list the most downloaded modules")
	}
	if body := get("/search?sort=downloads"); !strings.Contains(body, "3 downloads this month") {
		t.Error("search results should show monthly downloads")
	}
}
