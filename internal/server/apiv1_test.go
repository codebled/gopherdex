package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
)

func getJSON(t *testing.T, env *testEnv, path string, dst any) *http.Response {
	t.Helper()
	resp, body := fetch(t, env.srv, path)
	if dst != nil {
		if err := json.Unmarshal([]byte(body), dst); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, body)
		}
	}
	return resp
}

func TestBadges(t *testing.T) {
	env := newTestEnv(t)
	mod := "gopherdex.test/alice/retry"
	env.publish(t, mod, "v1.0.0")
	env.publish(t, mod, "v1.1.0")
	env.publish(t, mod, "v1.2.0-rc.1")

	for path, want := range map[string]string{
		"/badge/alice/retry.svg":                    `>v1.1.0</text>`, // the stable release, like go get @latest
		"/badge/alice/retry.svg?type=downloads":     `>0/month</text>`,
		"/badge/alice/retry.svg?type=verified":      `>not verified</text>`,
		"/badge/alice/retry.svg?type=security":      `>no known issues</text>`,
		"/badge/alice/retry.svg?label=go%20get":     `>go get</text>`,
		"/badge/alice/nothing.svg":                  `>not found</text>`,
		"/badge/alice/retry.svg?label=%3Cscript%3E": `>&lt;script&gt;</text>`,
	} {
		resp, body := fetch(t, env.srv, path)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/svg+xml; charset=utf-8" || !strings.Contains(body, want) {
			t.Errorf("%s: %d %s\n%s", path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	if resp, _ := fetch(t, env.srv, "/badge/alice/retry.svg?type=bogus"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown badge type: %d", resp.StatusCode)
	}

	// An advisory turns the security badge red.
	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})
	alice.post("/-/advisories", url.Values{"module": {mod}, "summary": {"Bug"}, "details": {"Details."}})
	if _, body := fetch(t, env.srv, "/badge/alice/retry.svg?type=security"); !strings.Contains(body, `>1 advisory</text>`) || !strings.Contains(body, "#CE3262") {
		t.Errorf("security badge with an advisory:\n%s", body)
	}

	_, body := fetch(t, env.srv, "/alice/retry")
	if !strings.Contains(body, `src="/badge/alice/retry.svg"`) || !strings.Contains(body, `[![retry](http://gopherdex.test/badge/alice/retry.svg)](http://gopherdex.test/alice/retry)`) {
		t.Error("project page lacks the README badge snippet")
	}
}

func TestAPIv1(t *testing.T) {
	env := newTestEnv(t)
	mod := "gopherdex.test/alice/retry"
	env.publish(t, mod, "v1.0.0")
	env.publish(t, mod, "v1.1.0")
	env.publishFiles(t, "gopherdex.test/alice/app", "v1.0.0", map[string]string{
		"go.mod": "module gopherdex.test/alice/app\n\nrequire " + mod + " v1.0.0\n", "app.go": "package app\n",
	})
	var aliceID int64
	env.accts.DB.QueryRow(`SELECT id FROM users WHERE username = 'alice'`).Scan(&aliceID)
	alice, err := env.accts.UserByID(context.Background(), aliceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Yank(context.Background(), alice, mod, "v1.0.0", "broken", accounts.Client{}); err != nil {
		t.Fatal(err)
	}

	var m v1Module
	resp := getJSON(t, env, "/api/v1/modules/alice/retry", &m)
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("no CORS header")
	}
	if m.Path != mod || m.Latest != "v1.1.0" || len(m.Versions) != 2 || !m.Versions[1].Yanked || m.Versions[1].YankReason != "broken" ||
		m.UsedBy != 1 || m.Owners[0] != "alice" || m.URLs.Badge != "http://gopherdex.test/badge/alice/retry.svg" || m.Synopsis != "Package lib is a test." {
		t.Errorf("module = %+v", m)
	}
	var full v1Module
	getJSON(t, env, "/api/v1/modules/"+mod, &full)
	if full.Path != mod {
		t.Error("full module path not accepted")
	}

	var v v1Version
	getJSON(t, env, "/api/v1/modules/alice/app/@v/v1.0.0", &v)
	if v.Version != "v1.0.0" || len(v.Requires) != 1 || v.Requires[0].Path != mod || !strings.HasPrefix(v.H1, "h1:") ||
		v.Provenance != nil || v.URLs.Zip == "" || len(v.Packages) != 1 {
		t.Errorf("version = %+v", v)
	}

	var e apiError
	if resp := getJSON(t, env, "/api/v1/modules/alice/missing", &e); resp.StatusCode != http.StatusNotFound || !strings.Contains(e.Error, "No gopherdex.test/alice/missing") {
		t.Errorf("missing module: %d %+v", resp.StatusCode, e)
	}
	if resp := getJSON(t, env, "/api/v1/modules/alice/retry/@v/v9.9.9", &e); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing version: %d", resp.StatusCode)
	}
	if resp := getJSON(t, env, "/api/v1/modules/alice/retry/@v/latest", &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad version: %d", resp.StatusCode)
	}

	var sr struct {
		Total   int
		Results []v1SearchHit
	}
	getJSON(t, env, "/api/v1/search?q=retry", &sr)
	if sr.Total != 1 || sr.Results[0].Path != mod || sr.Results[0].URL != "http://gopherdex.test/alice/retry" {
		t.Errorf("search = %+v", sr)
	}
	if resp := getJSON(t, env, "/api/v1/search?sort=nope", &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad sort: %d", resp.StatusCode)
	}

	var owner struct {
		Kind    string
		Modules []struct{ Path, Latest string }
	}
	getJSON(t, env, "/api/v1/owners/alice", &owner)
	if owner.Kind != "user" || len(owner.Modules) != 2 {
		t.Errorf("owner = %+v", owner)
	}
	var stats struct{ Modules, Releases int }
	getJSON(t, env, "/api/v1/stats", &stats)
	if stats.Modules != 2 || stats.Releases != 3 {
		t.Errorf("stats = %+v", stats)
	}

	resp, body := fetch(t, env.srv, "/api")
	expect(t, resp, body, http.StatusOK, "/api/v1/modules/alice/retry")
}
