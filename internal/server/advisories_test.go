package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	xmodule "golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
	"github.com/parthiban-sivakumar/gopherdex/internal/vulndb"
)

// fakeVulnDB serves a tiny public database: github.com/x/dep has
// GO-2020-0001 in versions before v1.5.0.
func fakeVulnDB(t *testing.T) *httptest.Server {
	files := map[string]string{
		"/index/db.json":      `{"modified":"2026-01-01T00:00:00Z"}`,
		"/index/modules.json": `[{"path":"github.com/x/dep","vulns":[{"id":"GO-2020-0001","modified":"2026-01-01T00:00:00Z","fixed":"1.5.0"}]}]`,
		"/index/vulns.json":   `[{"id":"GO-2020-0001","modified":"2026-01-01T00:00:00Z"}]`,
		"/ID/GO-2020-0001.json": `{"schema_version":"1.3.1","id":"GO-2020-0001","modified":"2026-01-01T00:00:00Z","published":"2026-01-01T00:00:00Z",
			"summary":"Header injection in dep","details":"x","affected":[{"package":{"name":"github.com/x/dep","ecosystem":"Go"},
			"ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"1.5.0"}]}]}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// publishGoMod publishes a version whose go.mod is gomod, as alice.
func (e *testEnv) publishGoMod(t *testing.T, modPath, version, gomod string) {
	t.Helper()
	ctx := context.Background()
	var id int64
	e.accts.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'alice'`).Scan(&id)
	u := &accounts.User{ID: id, Username: "alice", EmailVerified: true}
	_, tok, err := e.accts.CreateToken(ctx, u, "test", 0, "", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "go.mod"), []byte(gomod), 0o644)
	os.WriteFile(filepath.Join(src, "app.go"), []byte("// Package app is a test.\npackage app\n"), 0o644)
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

func TestSecurityAdvisories(t *testing.T) {
	upstream := &vulndb.Upstream{URL: fakeVulnDB(t).URL}
	if err := upstream.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	env := newTestEnvWith(t, func(c *Config) { c.VulnDB = upstream })
	mod := "gopherdex.test/alice/retry"
	for _, v := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		env.publish(t, mod, v)
	}
	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})

	resp, body := alice.get("/alice/retry?tab=security")
	expect(t, resp, body, http.StatusOK, "No security advisories have been published")
	expect(t, resp, body, http.StatusOK, "Publish a security advisory")

	form := url.Values{"module": {mod}, "summary": {""}, "details": {"Do retries forever."}, "introduced_0": {"v1.1.0"}, "fixed_0": {"v1.2.0"}}
	resp, body = alice.post("/-/advisories", form)
	expect(t, resp, body, http.StatusUnprocessableEntity, "Give a one-line summary")
	if !strings.Contains(body, "Do retries forever.") {
		t.Error("the form lost the details typed")
	}
	form.Set("summary", "Unbounded retries in retry.Do")
	form.Set("aliases", "CVE-2026-12345")
	form.Set("packages", ".: Do")
	resp, body = alice.post("/-/advisories", form)
	expect(t, resp, body, http.StatusSeeOther, "")
	id := regexp.MustCompile(`GDX-\d{4}-\d{4}`).FindString(resp.Header.Get("Location"))
	if id == "" {
		t.Fatalf("redirect to %q", resp.Header.Get("Location"))
	}
	env.mails.find(t, "alice@example.com", "Security advisory "+id+" published")

	resp, body = alice.get("/advisories/" + id + "?done=published")
	expect(t, resp, body, http.StatusOK, "Unbounded retries in retry.Do")
	expect(t, resp, body, http.StatusOK, "go get gopherdex.test/alice/retry@v1.2.0")
	expect(t, resp, body, http.StatusOK, "Save changes") // alice owns it
	if _, body := fetch(t, env.srv, "/advisories/"+id); strings.Contains(body, "Save changes") {
		t.Error("anonymous visitors see the edit form")
	}

	// Affected versions are flagged; the fixed one isn't.
	resp, body = fetch(t, env.srv, "/alice/retry@v1.1.0")
	expect(t, resp, body, http.StatusOK, "v1.1.0 has 1 known vulnerability.")
	if _, body := fetch(t, env.srv, "/alice/retry"); strings.Contains(body, "known vulnerabilit") {
		t.Error("the fixed latest version shows a vulnerability banner")
	}
	_, body = fetch(t, env.srv, "/alice/retry?tab=versions")
	if n := strings.Count(body, ">Vulnerable</span>"); n != 1 {
		t.Errorf("%d versions marked vulnerable, want 1 (v1.1.0)", n)
	}

	// The vulnerability database, as govulncheck reads it.
	_, body = fetch(t, env.srv, "/vulndb/index/modules.json")
	var modules []vulndb.ModulesEntry
	if err := json.Unmarshal([]byte(body), &modules); err != nil || len(modules) != 2 {
		t.Fatalf("modules.json = %s (%v)", body, err)
	}
	if modules[1].Path != mod || modules[1].Vulns[0].ID != id || modules[1].Vulns[0].Fixed != "1.2.0" {
		t.Errorf("our module in the index: %+v", modules[1])
	}
	resp, err := env.srv.Client().Get(env.srv.URL + "/vulndb/ID/" + id + ".json.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("ID/%s.json.gz isn't gzip: %v", id, err)
	}
	var entry vulndb.Entry
	if err := json.NewDecoder(zr).Decode(&entry); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if entry.ID != id || entry.Affected[0].Ranges[0].Events[1].Fixed != "1.2.0" || entry.Affected[0].EcosystemSpecific.Imports[0].Symbols[0] != "Do" {
		t.Errorf("OSV entry = %+v", entry)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noRedirect.Get(env.srv.URL + "/vulndb/ID/GO-2020-0001.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != upstream.URL+"/ID/GO-2020-0001.json.gz" {
		t.Errorf("public entry: %d → %q", resp.StatusCode, loc)
	}
	_, body = fetch(t, env.srv, "/vulndb/index/db.json")
	if !strings.Contains(body, `"modified":"20`) {
		t.Errorf("db.json = %s", body)
	}

	// Dependencies: both a public and a hosted vulnerable requirement.
	env.publishGoMod(t, "gopherdex.test/alice/app", "v1.0.0",
		"module gopherdex.test/alice/app\n\ngo 1.22\n\nrequire (\n\tgithub.com/x/dep v1.2.0\n\tgopherdex.test/alice/retry v1.1.0\n\tgithub.com/x/safe v1.0.0\n)\n")
	_, body = fetch(t, env.srv, "/alice/app?tab=security")
	for _, want := range []string{"2 known vulnerabilities in modules that v1.0.0 requires", "GO-2020-0001", "github.com/x/dep@v1.2.0", id, "Fixed in v1.5.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("app security tab doesn't mention %q", want)
		}
	}

	// Withdrawing clears the warnings.
	resp, body = alice.post("/-/advisories/"+id+"/withdraw", nil)
	expect(t, resp, body, http.StatusSeeOther, "")
	if _, body := fetch(t, env.srv, "/alice/retry@v1.1.0"); strings.Contains(body, "known vulnerabilit") {
		t.Error("banner shown for a withdrawn advisory")
	}
	_, body = fetch(t, env.srv, "/vulndb/ID/"+id+".json")
	if !strings.Contains(body, `"withdrawn":`) {
		t.Errorf("withdrawn entry lacks the withdrawn field: %s", body)
	}

	resp, body = fetch(t, env.srv, "/advisories")
	expect(t, resp, body, http.StatusOK, id)
	resp, body = fetch(t, env.srv, "/feeds/advisories.atom")
	expect(t, resp, body, http.StatusOK, id+": Unbounded retries in retry.Do (withdrawn)")
}
