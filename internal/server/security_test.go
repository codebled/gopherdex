package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	xmodule "golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
)

func (m *mailRecorder) find(t *testing.T, to, subject string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second) // some mail is sent in the background
	for time.Now().Before(deadline) {
		m.mu.Lock()
		for _, msg := range m.sent {
			if msg.To == to && strings.Contains(msg.Subject, subject) {
				m.mu.Unlock()
				return msg.Body
			}
		}
		m.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no email to %s about %q", to, subject)
	return ""
}

func TestPasswordResetFlow(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0") // registers alice
	b := newBrowser(t, env.srv.URL)

	resp, body := b.post("/forgot-password", url.Values{"email": {"nobody@example.com"}})
	expect(t, resp, body, http.StatusOK, "a reset link is on its way")
	resp, body = b.post("/forgot-password", url.Values{"email": {"alice@example.com"}})
	expect(t, resp, body, http.StatusOK, "a reset link is on its way")

	link := regexp.MustCompile(`/reset-password\?token=\S+`).FindString(env.mails.find(t, "alice@example.com", "Reset your Gopherdex password"))
	resp, body = b.get(link)
	expect(t, resp, body, http.StatusOK, "Choose a new password")
	token, _ := url.ParseQuery(strings.TrimPrefix(link, "/reset-password?"))
	resp, body = b.post("/reset-password", url.Values{"token": {token.Get("token")}, "password": {"short"}})
	expect(t, resp, body, http.StatusUnprocessableEntity, "at least 10 characters")
	resp, body = b.post("/reset-password", url.Values{"token": {token.Get("token")}, "password": {"a whole new password"}})
	expect(t, resp, body, http.StatusOK, "Your password is changed.")
	resp, body = b.get(link)
	expect(t, resp, body, http.StatusBadRequest, "invalid or has expired")

	resp, body = b.post("/login", url.Values{"login": {"alice"}, "password": {"a whole new password"}})
	expect(t, resp, body, http.StatusSeeOther, "")
}

func TestTwoFactorAdminAndReports(t *testing.T) {
	env := newTestEnv(t)
	mod := "gopherdex.test/alice/retry"
	env.publish(t, mod, "v1.0.0")
	env.accts.Register(context.Background(), "bob", "bob@example.com", "correct horse battery", accounts.Client{})

	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})

	// Admin tools need two-factor authentication.
	resp, body := alice.get("/admin")
	expect(t, resp, body, http.StatusForbidden, "Turn on two-factor authentication first.")

	// Turn it on from the security page.
	resp, body = alice.get("/account/security")
	expect(t, resp, body, http.StatusOK, `src="data:image/png;base64,`)
	secret := regexp.MustCompile(`<code class="secret">([A-Z2-7]+)</code>`).FindStringSubmatch(body)[1]
	now := time.Now()
	code, _ := accounts.TOTPCode(secret, now)
	resp, body = alice.post("/account/security/2fa", url.Values{"code": {code}})
	expect(t, resp, body, http.StatusOK, "Save these recovery codes")
	env.mails.find(t, "alice@example.com", "Two-factor authentication is on")

	// Sign in again: the password leads to the code step.
	alice.post("/logout", url.Values{})
	resp, body = alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}, "next": {"/admin"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login/2fa") {
		t.Fatalf("after password: Location %q", loc)
	}
	resp, body = alice.get("/account")
	expect(t, resp, body, http.StatusSeeOther, "") // not signed in yet
	resp, body = alice.post("/login/2fa", url.Values{"code": {"000000"}, "next": {"/admin"}})
	expect(t, resp, body, http.StatusUnauthorized, "That code isn")
	code, _ = accounts.TOTPCode(secret, now.Add(30*time.Second))
	resp, body = alice.post("/login/2fa", url.Values{"code": {code}, "next": {"/admin"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("after code: Location %q", loc)
	}
	resp, body = alice.get("/admin")
	expect(t, resp, body, http.StatusOK, "No open reports.")

	// Non-admins can't see the queue.
	bob := newBrowser(t, env.srv.URL)
	bob.post("/login", url.Values{"login": {"bob"}, "password": {"correct horse battery"}})
	resp, body = bob.get("/admin")
	expect(t, resp, body, http.StatusNotFound, "")

	// Bob reports the module; alice quarantines it.
	resp, body = bob.post("/report", url.Values{"module": {mod}, "category": {"malware"}, "details": {"init() downloads a binary."}})
	expect(t, resp, body, http.StatusOK, "your report was sent")
	resp, body = alice.get("/admin")
	expect(t, resp, body, http.StatusOK, "init() downloads a binary.")
	resp, body = alice.post("/-/admin/quarantine", url.Values{"module": {mod}, "reason": {"Confirmed malware."}})
	expect(t, resp, body, http.StatusSeeOther, "")
	env.mails.find(t, "alice@example.com", "is under review")

	anon := newBrowser(t, env.srv.URL)
	resp, body = anon.get("/alice/retry")
	expect(t, resp, body, http.StatusGone, "is unavailable")
	resp, body = anon.get("/api/proxy/" + mod + "/@v/list")
	expect(t, resp, body, http.StatusNotFound, "under review")
	resp, body = anon.get("/api/proxy/" + mod + "/@v/v1.0.0.zip")
	expect(t, resp, body, http.StatusNotFound, "")

	resp, body = alice.post("/-/admin/release", url.Values{"module": {mod}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = anon.get("/alice/retry")
	expect(t, resp, body, http.StatusOK, "Report this module")

	// Security log.
	resp, body = alice.get("/account/security")
	expect(t, resp, body, http.StatusOK, "2fa.enabled")
}

// upload publishes through the HTTP API, like the CLI does.
func upload(t *testing.T, env *testEnv, token, modPath, version string) (int, string) {
	t.Helper()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module "+modPath+"\n"), 0o644)
	os.WriteFile(filepath.Join(src, "lib.go"), []byte("// Package lib is a test.\npackage lib\n"), 0o644)
	var zipBuf bytes.Buffer
	if err := modzip.CreateFromDir(&zipBuf, xmodule.Version{Path: modPath, Version: version}, src); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("module", modPath)
	mw.WriteField("version", version)
	part, _ := mw.CreateFormFile("zip", "m.zip")
	part.Write(zipBuf.Bytes())
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/api/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := env.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestScopedTokenAndPublishEmail(t *testing.T) {
	env := newTestEnv(t)
	mod := "gopherdex.test/alice/retry"
	env.publish(t, mod, "v1.0.0")

	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})
	resp, body := alice.post("/account/tokens", url.Values{"name": {"ci"}, "expires": {"30"}, "module": {mod}})
	expect(t, resp, body, http.StatusCreated, "Copy it now")
	token := regexp.MustCompile(`gdx_[A-Za-z0-9_-]{43}`).FindString(body)
	env.mails.find(t, "alice@example.com", "New Gopherdex API token")
	resp, body = alice.get("/account")
	expect(t, resp, body, http.StatusOK, "<code>gopherdex.test/alice/retry</code>")
	resp, body = alice.post("/account/tokens", url.Values{"name": {"x"}, "expires": {"30"}, "module": {"gopherdex.test/bob/other"}})
	expect(t, resp, body, http.StatusUnprocessableEntity, "Choose one of your modules.")

	// The module token publishes its module, and every maintainer is told.
	if status, body := upload(t, env, token, mod, "v1.1.0"); status != http.StatusCreated {
		t.Fatalf("scoped upload: %d %s", status, body)
	}
	msg := env.mails.find(t, "alice@example.com", mod+" v1.1.0 was published")
	if !strings.Contains(msg, `API token "ci"`) || !strings.Contains(msg, "revoke the token") {
		t.Errorf("publish email:\n%s", msg)
	}
	// It can't publish anything else.
	if status, body := upload(t, env, token, "gopherdex.test/alice/other", "v1.0.0"); status != http.StatusForbidden || !strings.Contains(body, "token_scope") {
		t.Fatalf("scoped token on another module: %d %s", status, body)
	}
}
