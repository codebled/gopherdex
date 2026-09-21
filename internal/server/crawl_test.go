package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestCrawlAndHealth(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")

	resp, body := fetch(t, env.srv, "/robots.txt")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Disallow: /account") || !strings.Contains(body, "Sitemap: http://gopherdex.test/sitemap.xml") {
		t.Errorf("robots.txt: %d\n%s", resp.StatusCode, body)
	}
	resp, body = fetch(t, env.srv, "/sitemap.xml")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<loc>http://gopherdex.test/alice/retry</loc>") || !strings.Contains(body, "<lastmod>") {
		t.Errorf("sitemap.xml: %d\n%s", resp.StatusCode, body)
	}
	resp, body = fetch(t, env.srv, "/healthz")
	if resp.StatusCode != http.StatusOK || body != "ok\n" {
		t.Errorf("healthz: %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent for a plain-http site")
	}

	secure := newTestEnvWith(t, func(c *Config) { c.SecureCookies = true })
	if resp, _ := fetch(t, secure.srv, "/"); !strings.HasPrefix(resp.Header.Get("Strict-Transport-Security"), "max-age=") {
		t.Error("no HSTS on an https site")
	}

	env.reg.DB.Close()
	if resp, _ := fetch(t, env.srv, "/healthz"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("healthz with the database closed: %d", resp.StatusCode)
	}
}
