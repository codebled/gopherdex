package goproxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"example.com/hello/@v/v1.0.0/go.mod":   "module example.com/hello\n\ngo 1.21\n",
		"example.com/hello/@v/v1.0.0/hello.go": "package hello\n",
		"example.com/hello/@v/v1.0.0.json":     `{"Time":"2026-03-02T10:15:00Z","Origin":{"VCS":"git","URL":"https://example.com/hello.git","Hash":"4f1c2a9e","Ref":"refs/tags/v1.0.0"}}`,
		"example.com/hello/@v/v1.1.0/go.mod":   "module example.com/hello\n\ngo 1.21\n",
		"example.com/hello/@v/v1.1.0/hello.go": "package hello\n",
		"example.com/!caps/@v/v0.1.0/go.mod":   "module example.com/Caps\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(http.StripPrefix("/api/proxy", NewHandler(st, nil)))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestHandlerEndpoints(t *testing.T) {
	srv := newTestServer(t)
	base := srv.URL + "/api/proxy/"

	tests := []struct {
		path, status, contentType, body string
	}{
		{"example.com/hello/@v/list", "200 OK", "text/plain; charset=utf-8", "v1.0.0\nv1.1.0\n"},
		{"example.com/hello/@v/v1.0.0.mod", "200 OK", "text/plain; charset=utf-8", "module example.com/hello\n\ngo 1.21\n"},
		{"example.com/!caps/@v/list", "200 OK", "text/plain; charset=utf-8", "v0.1.0\n"},
		{"example.com/missing/@v/list", "404 Not Found", "text/plain; charset=utf-8", ""},
		{"example.com/hello/@v/v9.0.0.info", "404 Not Found", "text/plain; charset=utf-8", ""},
		{"example.com/hello/@v/v1.0.0.tar", "404 Not Found", "text/plain; charset=utf-8", ""},
		{"example.com/Hello/@v/list", "400 Bad Request", "text/plain; charset=utf-8", ""},
		{"example.com/hello/@v/1.0.0.info", "400 Bad Request", "text/plain; charset=utf-8", ""},
	}
	for _, tt := range tests {
		resp, body := get(t, base+tt.path)
		if resp.Status != tt.status {
			t.Errorf("GET %s: status %s, want %s (%s)", tt.path, resp.Status, tt.status, body)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != tt.contentType {
			t.Errorf("GET %s: Content-Type %q, want %q", tt.path, ct, tt.contentType)
		}
		if tt.body != "" && string(body) != tt.body {
			t.Errorf("GET %s: body %q, want %q", tt.path, body, tt.body)
		}
	}
}

func TestHandlerInfoAndLatest(t *testing.T) {
	srv := newTestServer(t)
	for path, want := range map[string]string{
		"example.com/hello/@v/v1.0.0.info": "v1.0.0",
		"example.com/hello/@latest":        "v1.1.0",
	} {
		resp, body := get(t, srv.URL+"/api/proxy/"+path)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("GET %s: %s %s", path, resp.Status, resp.Header.Get("Content-Type"))
		}
		var info module.Info
		if err := json.Unmarshal(body, &info); err != nil {
			t.Fatal(err)
		}
		if info.Version != want || info.Time.IsZero() {
			t.Errorf("GET %s: info %+v, want version %s", path, info, want)
		}
	}
	_, body := get(t, srv.URL+"/api/proxy/example.com/hello/@v/v1.0.0.info")
	if !strings.Contains(string(body), `"Hash":"4f1c2a9e"`) {
		t.Errorf("info is missing the commit hash: %s", body)
	}
}

func TestHandlerZip(t *testing.T) {
	srv := newTestServer(t)
	resp, body := get(t, srv.URL+"/api/proxy/example.com/hello/@v/v1.1.0.zip")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if want := []string{"example.com/hello@v1.1.0/go.mod", "example.com/hello@v1.1.0/hello.go"}; !slices.Equal(names, want) {
		t.Fatalf("zip names = %v, want %v", names, want)
	}
}

func TestClientAgainstHandler(t *testing.T) {
	srv := newTestServer(t)
	c := NewClient(srv.URL+"/api/proxy", srv.Client())
	ctx := context.Background()

	versions, err := c.Versions(ctx, "example.com/hello")
	if err != nil || !slices.Equal(versions, []string{"v1.0.0", "v1.1.0"}) {
		t.Fatalf("Versions = %v, %v", versions, err)
	}
	info, err := c.Info(ctx, "example.com/hello", "v1.0.0")
	if err != nil || info.Origin == nil || info.Origin.Hash != "4f1c2a9e" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	latest, err := c.Latest(ctx, "example.com/hello")
	if err != nil || latest.Version != "v1.1.0" {
		t.Fatalf("Latest = %+v, %v", latest, err)
	}
	if _, err := c.GoMod(ctx, "example.com/missing", "v1.0.0"); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("GoMod(missing) = %v, want ErrNotFound", err)
	}
}

// closeTracker is a zip that records whether it was closed.
type closeTracker struct{ closed bool }

func (z *closeTracker) WriteTo(w io.Writer) (int64, error) { z.closed = true; return 0, nil }
func (z *closeTracker) Close() error                       { z.closed = true; return nil }

type zipSource struct {
	Source
	zip *closeTracker
}

func (s zipSource) Zip(context.Context, string, string) (io.WriterTo, error) { return s.zip, nil }

func TestHeadZipClosesIt(t *testing.T) {
	z := &closeTracker{}
	h := NewHandler(zipSource{zip: z}, nil)
	req := httptest.NewRequest(http.MethodHead, "/example.com/hello/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !z.closed {
		t.Fatalf("HEAD: status %d, closed %v", rec.Code, z.closed)
	}
}

func TestCacheBoundedByBytes(t *testing.T) {
	c := &ttlCache{items: map[string]cacheItem{}, max: 100, maxBytes: 3 << 20}
	c.put("huge", make([]byte, maxCachedBody+1), time.Hour)
	if _, ok := c.get("huge"); ok {
		t.Error("a body over the per-item limit was cached")
	}
	for i := range 5 {
		c.put(fmt.Sprint(i), make([]byte, 1<<20), time.Hour)
		if c.bytes > c.maxBytes {
			t.Fatalf("cache holds %d bytes, limit %d", c.bytes, c.maxBytes)
		}
	}
	c.put("0", make([]byte, 10), time.Hour) // replacing counts the new size only
	total := 0
	for _, it := range c.items {
		total += len(it.body)
	}
	if total != c.bytes {
		t.Errorf("tracked %d bytes, holds %d", c.bytes, total)
	}
}
