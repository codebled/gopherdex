package server

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestAssetFingerprints(t *testing.T) {
	env := newTestEnv(t)
	_, page := fetch(t, env.srv, "/")
	for _, name := range []string{"app.css", "app.js", "favicon.svg", "apple-touch-icon.png", "site.webmanifest"} {
		if !regexp.MustCompile(`/static/` + regexp.QuoteMeta(name) + `\?v=[0-9a-f]{10}"`).MatchString(page) {
			t.Errorf("page doesn't link a fingerprinted %s", name)
		}
	}
	for _, want := range []string{`href="/favicon.ico?v=`, `href="/tokens.css?v=`, `<meta property="og:image" content="http://gopherdex.test/static/og-image.png?v=`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}

	css := regexp.MustCompile(`/static/app\.css\?v=[0-9a-f]+`).FindString(page)
	resp, _ := fetch(t, env.srv, css)
	if resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("fingerprinted asset: Cache-Control %q", resp.Header.Get("Cache-Control"))
	}
	resp, _ = fetch(t, env.srv, "/static/app.css")
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("unversioned asset: Cache-Control %q", resp.Header.Get("Cache-Control"))
	}
	resp, _ = fetch(t, env.srv, "/static/app.css?v=stale")
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Error("an old fingerprint is cached for a year")
	}
	etag := resp.Header.Get("ETag")
	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/static/app.css", nil)
	req.Header.Set("If-None-Match", etag)
	if r, err := env.srv.Client().Do(req); err != nil || r.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation: %v %v", r.StatusCode, err)
	}
	resp, _ = fetch(t, env.srv, "/static/site.webmanifest")
	if resp.Header.Get("Content-Type") != "application/manifest+json" {
		t.Errorf("manifest type %q", resp.Header.Get("Content-Type"))
	}
	resp, _ = fetch(t, env.srv, "/favicon.ico")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/x-icon" {
		t.Errorf("favicon.ico: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
