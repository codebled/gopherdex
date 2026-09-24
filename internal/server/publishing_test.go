package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codebled/gopherdex/internal/accounts"
	"github.com/codebled/gopherdex/internal/oidc"
	"github.com/codebled/gopherdex/internal/oidc/oidctest"
)

func postJSON(t *testing.T, env *testEnv, path, token string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := env.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestTrustedPublishingFlow(t *testing.T) {
	iss := oidctest.New(t)
	env := newTestEnvWith(t, func(c *Config) {
		c.GitHubOIDC = &oidc.Verifier{Issuer: iss.URL, Audience: "gopherdex.test"}
	})
	mod := "gopherdex.test/alice/retry"
	env.publish(t, mod, "v1.0.0")
	if _, err := env.accts.Register(context.Background(), "bob", "bob@example.com", "correct horse battery", accounts.Client{}); err != nil {
		t.Fatal(err)
	}

	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})

	// Owners add publishers on the Manage tab.
	resp, body := alice.get("/alice/retry?tab=manage")
	expect(t, resp, body, http.StatusOK, "Trusted publishers")
	resp, body = alice.post("/-/publishers", url.Values{"module": {mod}, "repository": {"alice/retry"}, "workflow": {"release.sh"}})
	expect(t, resp, body, http.StatusBadRequest, "Give the workflow")
	resp, body = alice.post("/-/publishers", url.Values{"module": {mod}, "repository": {"https://github.com/Alice/retry"}, "workflow": {"release.yml"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "tab=manage&done=publisher-added") {
		t.Errorf("redirect to %q", loc)
	}
	resp, body = alice.get("/alice/retry?tab=manage&done=publisher-added")
	expect(t, resp, body, http.StatusOK, "<code>alice/retry</code>")
	mail := env.mails.find(t, "alice@example.com", "Trusted publisher added for "+mod)
	if !strings.Contains(mail, "release.yml in https://github.com/alice/retry") {
		t.Errorf("publisher email:\n%s", mail)
	}
	pubID := regexp.MustCompile(`name="id" value="(\d+)"`).FindStringSubmatch(body)[1]

	// Pending publishers live on the account page.
	resp, body = alice.post("/-/publishers", url.Values{"from": {"account"}, "module": {"gopherdex.test/bob/stolen"}, "repository": {"alice/x"}, "workflow": {"release.yml"}})
	expect(t, resp, body, http.StatusForbidden, "can&#39;t create modules in gopherdex.test/bob/")
	if !strings.Contains(body, `value="gopherdex.test/bob/stolen"`) {
		t.Error("the form lost what was typed")
	}
	resp, _ = alice.post("/-/publishers", url.Values{"from": {"account"}, "module": {"gopherdex.test/alice/next"}, "repository": {"alice/next"}, "workflow": {"release.yml"}})
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/account?done=publisher-added") {
		t.Errorf("pending publisher redirect to %q", loc)
	}
	resp, body = alice.get("/account")
	expect(t, resp, body, http.StatusOK, "Waiting for first release")

	// The exchange.
	_, body = fetch(t, env.srv, "/api/oidc/audience")
	if !strings.Contains(body, `"audience":"gopherdex.test"`) {
		t.Errorf("audience: %s", body)
	}
	if status, body := postJSON(t, env, "/api/oidc/mint-token", "", map[string]string{"token": "junk", "module": mod}); status != http.StatusUnauthorized {
		t.Errorf("junk ID token: %d %s", status, body)
	}
	idToken := iss.Sign(t, iss.GitHubClaims("alice/retry", "release.yml", "gopherdex.test", time.Now().Unix()))
	status, body := postJSON(t, env, "/api/oidc/mint-token", "", map[string]string{"token": idToken, "module": mod})
	if status != http.StatusOK {
		t.Fatalf("mint: %d %s", status, body)
	}
	var minted struct{ Token, Publisher string }
	json.Unmarshal([]byte(body), &minted)
	if minted.Publisher != "GitHub Actions: alice/retry (release.yml)" {
		t.Errorf("publisher = %q", minted.Publisher)
	}
	if status, body := postJSON(t, env, "/api/oidc/mint-token", "", map[string]string{"token": idToken, "module": mod}); status != http.StatusForbidden || !strings.Contains(body, "already exchanged") {
		t.Errorf("reused ID token: %d %s", status, body)
	}

	if status, body := upload(t, env, minted.Token, mod, "v1.1.0"); status != http.StatusCreated {
		t.Fatalf("trusted upload: %d %s", status, body)
	}
	mail = env.mails.find(t, "alice@example.com", mod+" v1.1.0 was published")
	if !strings.Contains(mail, "The GitHub Actions workflow release.yml in https://github.com/alice/retry published") ||
		!strings.Contains(mail, "/actions/runs/42") {
		t.Errorf("publish email:\n%s", mail)
	}
	resp, body = alice.get("/alice/retry@v1.1.0")
	expect(t, resp, body, http.StatusOK, "Verified source")
	resp, body = alice.get("/alice/retry?tab=versions")
	expect(t, resp, body, http.StatusOK, `href="https://github.com/alice/retry/actions/runs/42" rel="nofollow noopener">GitHub Actions</a>`)

	// Minted tokens only upload.
	if status, body := postJSON(t, env, "/api/yank", minted.Token, map[string]any{"module": mod, "version": "v1.0.0", "yank": true}); status != http.StatusForbidden {
		t.Errorf("yank with minted token: %d %s", status, body)
	}

	// Removing the publisher revokes its tokens.
	resp, body = alice.post("/-/publishers/remove", url.Values{"module": {mod}, "id": {pubID}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if status, _ := upload(t, env, minted.Token, mod, "v1.2.0"); status != http.StatusUnauthorized {
		t.Errorf("upload after removal: %d", status)
	}
	env.mails.find(t, "alice@example.com", "Trusted publisher removed for "+mod)

	// Other people can't remove publishers.
	bob := newBrowser(t, env.srv.URL)
	env.accts.DB.Exec(`UPDATE users SET email_verified_at = 1 WHERE username = 'bob'`)
	bob.post("/login", url.Values{"login": {"bob"}, "password": {"correct horse battery"}})
	_, body = alice.get("/account")
	pendingID := regexp.MustCompile(`name="id" value="(\d+)"`).FindStringSubmatch(body)[1]
	resp, body = bob.post("/-/publishers/remove", url.Values{"from": {"account"}, "id": {pendingID}})
	expect(t, resp, body, http.StatusForbidden, "")
}

func TestTrustedPublishingOff(t *testing.T) {
	env := newTestEnv(t)
	if resp, _ := fetch(t, env.srv, "/api/oidc/audience"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("audience with trusted publishing off: %d", resp.StatusCode)
	}
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})
	if _, body := alice.get("/alice/retry?tab=manage"); strings.Contains(body, "Trusted publishers") {
		t.Error("manage tab offers trusted publishers while they're off")
	}
}
