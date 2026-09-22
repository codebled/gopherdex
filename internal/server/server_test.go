package server

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/blob"
	"github.com/parthiban-sivakumar/gopherdex/internal/database"
	"github.com/parthiban-sivakumar/gopherdex/internal/discovery"
	"github.com/parthiban-sivakumar/gopherdex/internal/goproxy"
	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
	"github.com/parthiban-sivakumar/gopherdex/internal/store"
	"github.com/parthiban-sivakumar/gopherdex/web"
)

// newTestServer serves the sample modules in data/modules, offline, with a
// fresh database. Emails are captured by the returned recorder.
func newTestServer(t *testing.T) *httptest.Server {
	return newTestEnv(t).srv
}

func newTestServerWithMail(t *testing.T) (*httptest.Server, *mailRecorder) {
	env := newTestEnv(t)
	return env.srv, env.mails
}

type testEnv struct {
	srv       *httptest.Server
	mails     *mailRecorder
	accts     *accounts.Service
	reg       *registry.Registry
	downloads *registry.Downloads
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, nil)
}

// newTestEnvWith is newTestEnv with a hook to adjust the configuration.
func newTestEnvWith(t *testing.T, configure func(*Config)) *testEnv {
	t.Helper()
	st, err := store.Open("../../data/modules")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	blobs, err := blob.OpenFS(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &mailRecorder{}
	reg := &registry.Registry{DB: db, Blobs: blobs, ModuleHost: "gopherdex.test", MaxZipSize: 1 << 20, Log: log}
	hosted := registry.Multi{reg, st}
	disc := &discovery.Service{Local: hosted, ProxyPrefix: "/api/proxy", DocsBase: "https://pkg.go.dev", Log: log, ModuleHost: "gopherdex.test"}
	accts := &accounts.Service{DB: db, Mailer: rec, BaseURL: "http://gopherdex.test", Log: log}
	downloads := &registry.Downloads{Registry: reg}
	proxy := goproxy.NewHandler(hosted, log)
	proxy.OnZip = downloads.Count
	cfg := Config{
		Discovery: disc, Accounts: accts, Registry: reg, Proxy: proxy, Log: log,
		SiteURL: "http://gopherdex.test", ModuleHost: "gopherdex.test", Admins: []string{"alice"},
	}
	if configure != nil {
		configure(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, mails: rec, accts: accts, reg: reg, downloads: downloads}
}

type mailRecorder struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (m *mailRecorder) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *mailRecorder) last(t *testing.T) mail.Message {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("no email sent")
	}
	return m.sent[len(m.sent)-1]
}

func fetch(t *testing.T, srv *httptest.Server, path string, header ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestRoutes(t *testing.T) {
	srv := newTestServer(t)
	tests := []struct {
		path        string
		status      int
		contentType string
		contains    string
	}{
		{"/", 200, "text/html; charset=utf-8", "No modules yet."},
		{"/modules", 200, "text/html; charset=utf-8", "No modules have been published yet."}, // redirects to /search
		{"/search", 200, "text/html; charset=utf-8", "No modules have been published yet."},
		{"/search?q=anything&license=MIT", 200, "text/html; charset=utf-8", "with these filters"},
		{"/search?page=2", 404, "text/html; charset=utf-8", ""},
		{"/?q=retry", 200, "text/html; charset=utf-8", "retry"},            // old home-page searches redirect
		{"/api/search?q=greetings", 200, "application/json", `"results":`}, // hosted results come from the index: see TestAPISearch
		{"/api/search?q=nothing-matches-this", 200, "application/json", `"results":[]`},
		{"/api/modules/example.com/hello", 200, "application/json", `"latest":"v1.1.0"`},
		{"/api/modules/example.com/hello?version=v1.2.0-beta.1", 200, "application/json", `"importPath":"example.com/hello/wave"`},
		{"/api/modules/example.com/missing", 404, "application/json", `"error"`},
		{"/api/modules/example.com/hello?version=banana", 400, "application/json", `"error"`},
		{"/api/modules/nodomain/x", 400, "application/json", `"error"`},
		{"/api/proxy/example.com/hello/@v/list", 200, "text/plain; charset=utf-8", "v1.0.0\nv1.1.0\nv1.2.0-beta.1\n"},
		{"/api/proxy/example.com/hello/@v/v1.1.0.info", 200, "application/json", `"Hash":"9b2e61c04d7a83f5e2b1c9d0a4f3e7b6c5d8a291"`},
		{"/api/proxy/example.com/hello/@v/v1.1.0.zip", 200, "application/zip", "example.com/hello@v1.1.0/hello.go"},
		{"/tokens.css", 200, "text/css; charset=utf-8", "--go-gopher-blue"},
		{"/static/app.js", 200, "text/javascript; charset=utf-8", "Gopherdex"},
		{"/healthz", 200, "text/plain; charset=utf-8", "ok"},
		{"/favicon.ico", 200, "image/x-icon", ""},
		{"/static/favicon.svg", 200, "image/svg+xml", "#00ADD8"},
		{"/static/apple-touch-icon.png", 200, "image/png", ""},
		{"/nope", 404, "text/html; charset=utf-8", "nothing at this address"},
	}
	for _, tt := range tests {
		resp, body := fetch(t, srv, tt.path)
		if resp.StatusCode != tt.status {
			t.Errorf("GET %s: status %d, want %d\n%s", tt.path, resp.StatusCode, tt.status, body)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != tt.contentType {
			t.Errorf("GET %s: Content-Type %q, want %q", tt.path, ct, tt.contentType)
		}
		if !strings.Contains(body, tt.contains) {
			t.Errorf("GET %s: body does not contain %q", tt.path, tt.contains)
		}
	}
}

func TestModuleHistory(t *testing.T) {
	srv := newTestServer(t)
	_, body := fetch(t, srv, "/api/modules/example.com/hello")
	var m discovery.Module
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Versions) != 3 || m.Versions[0].Version != "v1.2.0-beta.1" {
		t.Fatalf("versions = %+v", m.Versions)
	}
	if !m.Versions[2].Retracted {
		t.Errorf("v1.0.0 should be retracted: %+v", m.Versions[2])
	}
	if m.Origin != discovery.Hosted || len(m.Packages) != 1 || m.Readme == "" {
		t.Errorf("module = %+v", m)
	}
}

// The site uses only the Go brand colors, so every color in the stylesheet,
// script and template must come from a token in /tokens.css.
func TestNoColorLiterals(t *testing.T) {
	literal := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\b(rgba?|hsla?|color-mix)\(`)
	names := []string{"static/app.css", "static/app.js"}
	for _, pattern := range []string{"templates/*/*.html"} {
		matches, err := fs.Glob(web.FS, pattern)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, matches...)
	}
	for _, name := range names {
		data, err := fs.ReadFile(web.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, `name="theme-color"`) {
				continue // must be a literal; it is Go white
			}
			if m := literal.FindString(line); m != "" {
				t.Errorf("%s:%d uses color %q; use a token from internal/tokens instead", name, i+1, m)
			}
		}
	}
}

func TestTokensETag(t *testing.T) {
	srv := newTestServer(t)
	resp, _ := fetch(t, srv, "/tokens.css")
	tag := resp.Header.Get("ETag")
	if tag == "" {
		t.Fatal("no ETag")
	}
	resp, _ = fetch(t, srv, "/tokens.css", "If-None-Match", tag)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET: status %d, want 304", resp.StatusCode)
	}
}

func TestAPISearch(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	for _, q := range []string{"/api/search?q=retry", "/api/search?q=retry&scope=hosted"} {
		resp, body := fetch(t, env.srv, q)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"path":"gopherdex.test/alice/retry"`) {
			t.Errorf("%s: %d %s", q, resp.StatusCode, body)
		}
	}
}
