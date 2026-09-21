package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/oidc"
	"github.com/parthiban-sivakumar/gopherdex/internal/oidc/oidctest"
	"github.com/parthiban-sivakumar/gopherdex/internal/server"
)

// TestTrustedPublish runs the whole flow: a GitHub Actions job with no API
// token publishes through a trusted publisher, and the project page shows
// the verified source.
func TestTrustedPublish(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	iss := oidctest.New(t)
	tr := startRegistryWith(t, func(c *server.Config) {
		c.GitHubOIDC = &oidc.Verifier{Issuer: iss.URL, Audience: moduleHost}
	})
	mod := moduleHost + "/alice/retry"
	if _, err := tr.reg.AddPublisher(context.Background(), tr.alice, mod, "alice/retry", "release.yml", "", accounts.Client{}); err != nil {
		t.Fatal(err)
	}

	// GitHub's ID token endpoint, issuing tokens for workflow.
	workflow := "release.yml"
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "bearer runtime-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims := iss.GitHubClaims("alice/retry", workflow, r.URL.Query().Get("audience"), time.Now().Unix())
		json.NewEncoder(w).Encode(map[string]string{"value": iss.Sign(t, claims)})
	}))
	t.Cleanup(gh.Close)

	repo := gitRepo(t, mod)
	gitRun(t, repo, "tag", "v1.0.0")
	env := map[string]string{
		"GOPHERDEX_CONFIG":               filepath.Join(t.TempDir(), "credentials.json"), // no saved token
		"GOPHERDEX_REGISTRY":             tr.srv.URL,
		"GITHUB_ACTIONS":                 "true",
		"ACTIONS_ID_TOKEN_REQUEST_URL":   gh.URL + "/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "runtime-token",
	}
	r := &runner{t: t, env: env, dir: repo, client: tr.srv.Client()}

	code, stdout, stderr := r.run("", "publish")
	if code != 0 {
		t.Fatalf("trusted publish: exit %d\n%s%s", code, stdout, stderr)
	}
	mustContain(t, stdout, "auth     trusted publisher, GitHub Actions: alice/retry (release.yml)")
	mustContain(t, stdout, "Published "+mod+"@v1.0.0")

	resp, err := tr.srv.Client().Get(tr.srv.URL + "/alice/retry")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	mustContain(t, string(page), "Verified source")
	mustContain(t, string(page), `href="https://github.com/alice/retry/actions/runs/42"`)

	// A workflow the owners didn't name is refused, with the reason.
	workflow = "pr-checks.yml"
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "next")
	gitRun(t, repo, "tag", "v1.1.0")
	code, _, stderr = r.run("", "publish")
	if code != 1 {
		t.Fatalf("publish from unlisted workflow: exit %d", code)
	}
	mustContain(t, stderr, "No trusted publisher of "+mod+" matches this run")
	mustContain(t, stderr, "workflow pr-checks.yml")

	// A job without id-token: write gets told how to fix it.
	delete(env, "ACTIONS_ID_TOKEN_REQUEST_URL")
	delete(env, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	code, _, stderr = r.run("", "publish")
	if code != 1 {
		t.Fatalf("publish without id-token permission: exit %d", code)
	}
	mustContain(t, stderr, "id-token: write")
}

// TestPublishWarnings shows the registry's publish-check warnings.
func TestPublishWarnings(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv, token := startRegistry(t)
	mod := moduleHost + "/alice/retry"
	repo := gitRepo(t, mod)
	if err := os.WriteFile(filepath.Join(repo, "init.go"), []byte("package retry\n\nimport \"os/exec\"\n\nfunc init() { exec.Command(\"id\").Run() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	gitRun(t, repo, "tag", "v1.0.0")
	r := &runner{t: t, env: map[string]string{"GOPHERDEX_CONFIG": filepath.Join(t.TempDir(), "c.json"), "GOPHERDEX_REGISTRY": srv.URL, "GOPHERDEX_TOKEN": token}, dir: repo, client: srv.Client()}
	code, stdout, stderr := r.run("", "publish")
	if code != 0 {
		t.Fatalf("publish: %d %s", code, stderr)
	}
	mustContain(t, stdout, "Published "+mod+"@v1.0.0")
	mustContain(t, stdout, "flagged 1 thing(s)")
	mustContain(t, stdout, "warning  init() calls os/exec.Command, which starts a process as soon as a program imports the package. (init.go:5)")
}
