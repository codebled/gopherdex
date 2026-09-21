package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/discovery"
)

// fakePkgsite answers pkg.go.dev's /v1/search, counting requests; delay
// makes it slow.
func fakePkgsite(t *testing.T, delay time.Duration, hits *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, `{"items": [
			{"packagePath": "github.com/avast/retry-go", "modulePath": "github.com/avast/retry-go", "version": "v4.6.0", "synopsis": "Simple library for retry mechanism"},
			{"packagePath": "gopherdex.test/alice/retry", "modulePath": "gopherdex.test/alice/retry", "version": "v1.0.0", "synopsis": "the hosted one, seen by the public index"},
			{"packagePath": "github.com/cenkalti/backoff/v4", "modulePath": "github.com/cenkalti/backoff/v4", "version": "v4.3.0", "synopsis": "Exponential backoff with retry"}
		]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUnifiedSearch(t *testing.T) {
	var calls atomic.Int32
	pkgsite := fakePkgsite(t, 0, &calls)
	env := newTestEnvWith(t, func(c *Config) {
		c.Discovery.Index = &discovery.PkgsiteIndex{Base: pkgsite.URL, HTTP: pkgsite.Client()}
	})
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	env.publish(t, "gopherdex.test/alice/other", "v1.0.0")

	resp, body := fetch(t, env.srv, "/search?q=retry")
	expect(t, resp, body, http.StatusOK, `aria-current="page">All Go modules</a>`)
	expect(t, resp, body, http.StatusOK, "1 on Gopherdex · 2 from pkg.go.dev")
	hosted := strings.Index(body, `>gopherdex.test/<wbr>alice/<wbr>retry</code>`)
	public := strings.Index(body, "github.com/<wbr>avast/<wbr>retry-go")
	if hosted < 0 || public < 0 || hosted > public {
		t.Errorf("hosted exact match should come first (hosted at %d, public at %d)", hosted, public)
	}
	if n := strings.Count(body, "alice/<wbr>retry</code>"); n != 1 {
		t.Errorf("hosted module listed %d times; the public duplicate should be dropped", n)
	}
	if !strings.Contains(body, `<span class="pill p-hosted">Gopherdex</span>`) || !strings.Contains(body, `<span class="pill p-public">pkg.go.dev</span>`) {
		t.Error("results aren't labeled by source")
	}

	// The same query again comes from the cache.
	fetch(t, env.srv, "/search?q=retry")
	if calls.Load() != 1 {
		t.Errorf("pkg.go.dev asked %d times for one query", calls.Load())
	}

	// This registry only.
	resp, body = fetch(t, env.srv, "/search?q=retry&scope=hosted")
	expect(t, resp, body, http.StatusOK, "1 result for")
	if strings.Contains(body, "avast") {
		t.Error("hosted scope shows public modules")
	}
	// Filters apply to hosted modules only, and say so.
	resp, body = fetch(t, env.srv, "/search?q=retry&hide_deprecated=1")
	expect(t, resp, body, http.StatusOK, "Filters, sorting and pages apply to modules on Gopherdex")
	if strings.Contains(body, "avast") {
		t.Error("filtered search shows public modules")
	}
	// Browsing without a query lists hosted modules.
	resp, body = fetch(t, env.srv, "/search")
	expect(t, resp, body, http.StatusOK, "2 modules")
}

func TestUnifiedSearchSlowUpstream(t *testing.T) {
	var calls atomic.Int32
	pkgsite := fakePkgsite(t, 5*time.Second, &calls)
	env := newTestEnvWith(t, func(c *Config) {
		c.Discovery.Index = &discovery.PkgsiteIndex{Base: pkgsite.URL, HTTP: pkgsite.Client()}
	})
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")

	start := time.Now()
	resp, body := fetch(t, env.srv, "/search?q=retry")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("search waited %v on a slow pkg.go.dev", took)
	}
	expect(t, resp, body, http.StatusOK, "load in time, so only modules on Gopherdex are shown")
	expect(t, resp, body, http.StatusOK, "alice/<wbr>retry")
}
