package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xmodule "golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

// publishFiles publishes a version with the given files, as alice.
func (e *testEnv) publishFiles(t *testing.T, modPath, version string, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	u, err := e.accts.Register(ctx, "alice", "alice@example.com", "correct horse battery", accounts.Client{})
	if err != nil && err != accounts.ErrUsernameTaken {
		t.Fatal(err)
	}
	e.accts.DB.ExecContext(ctx, `UPDATE users SET email_verified_at = 1 WHERE username = 'alice'`)
	if u == nil {
		u = &accounts.User{Username: "alice"}
		e.accts.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'alice'`).Scan(&u.ID)
	}
	u.EmailVerified = true
	_, tok, err := e.accts.CreateToken(ctx, u, "test", 0, "", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for name, body := range files {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
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

func TestDocsSourceAndDependencies(t *testing.T) {
	play := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/share" || !strings.Contains(string(body), "package main") {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		io.WriteString(w, "AbC123")
	}))
	t.Cleanup(play.Close)
	env := newTestEnvWith(t, func(c *Config) { c.PlaygroundURL = play.URL })

	mod := "gopherdex.test/alice/retry"
	env.publishFiles(t, mod, "v1.0.0", map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.22\n",
		"retry.go": `// Package retry retries operations. See [Do].
package retry

// MaxAttempts caps retries.
const MaxAttempts = 10

// Do runs fn.
func Do(fn func() error) error { return fn() }

// Forever retries forever.
//
// Deprecated: Use [Do].
func Forever() {}
`,
		"example_test.go": "package retry_test\n\nimport (\n\t\"fmt\"\n\n\t\"" + mod + "\"\n)\n\nfunc ExampleDo() {\n\tfmt.Println(retry.Do(func() error { return nil }))\n\t// Output: <nil>\n}\n",
	})
	app := "gopherdex.test/alice/app"
	env.publishFiles(t, app, "v1.0.0", map[string]string{
		"go.mod": "module " + app + "\n\ngo 1.22\n\nrequire (\n\t" + mod + " v1.0.0\n\tgithub.com/x/y v1.2.3 // indirect\n)\n",
		"app.go": "package app\n",
	})

	_, body := fetch(t, env.srv, "/alice/retry?tab=docs")
	for _, want := range []string{
		`id="pkg.Do"`, `<a href="#pkg.Do">func Do</a>`, `See <a href="#pkg.Do">Do</a>`,
		`href="/alice/retry?tab=source&amp;file=retry.go#L8">retry.go:8</a>`,
		`const MaxAttempts`, `>Deprecated</span>`, `Output:`, `Run in the Go Playground`, `data-sym-filter`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("docs tab lacks %s", want)
		}
	}

	_, body = fetch(t, env.srv, "/alice/retry?tab=source&file=retry.go")
	if !strings.Contains(body, `<tr id="L8"><td class="ln"><a href="#L8">8</a></td><td class="lc">func Do(fn func() error) error { return fn() }</td></tr>`) {
		t.Errorf("source view:\n%s", body[strings.Index(body, "source-layout"):][:1500])
	}
	if _, body := fetch(t, env.srv, "/alice/retry?tab=source&file=../../etc/passwd"); !strings.Contains(body, "Choose a file.") {
		t.Error("a path outside the zip wasn't refused")
	}

	_, body = fetch(t, env.srv, "/alice/retry?tab=deps")
	if !strings.Contains(body, "Used by 1 module") || !strings.Contains(body, `<a href="/alice/app"><code>gopherdex.test/alice/app</code></a>`) {
		t.Error("retry's dependents aren't listed")
	}
	_, body = fetch(t, env.srv, "/alice/app?tab=deps")
	if !strings.Contains(body, `<a href="/alice/retry"><code>gopherdex.test/alice/retry</code></a>`) ||
		!strings.Contains(body, `href="https://pkg.go.dev/github.com/x/y"`) || !strings.Contains(body, "1 indirect dependency") {
		t.Error("app's requirements aren't listed")
	}

	// Run an example in the playground.
	b := newBrowser(t, env.srv.URL)
	resp, _ := b.post("/-/play", url.Values{"code": {"package main\n\nfunc main() {}\n"}})
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "https://go.dev/play/p/AbC123" {
		t.Errorf("play: %d → %q", resp.StatusCode, loc)
	}
	if resp, _ := b.post("/-/play", url.Values{"code": {"rm -rf /"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-program shared: %d", resp.StatusCode)
	}
}
