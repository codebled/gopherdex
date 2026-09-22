package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/parthiban-sivakumar/gopherdex/internal/webauthntest"
)

// postJSONAs posts JSON with the browser's cookies, as the page script does.
func (b *browser) postJSONAs(path string, body []byte) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestPasskeysOverHTTP(t *testing.T) {
	env := newTestEnv(t) // base URL http://gopherdex.test
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	key := webauthntest.NewSoftKey(t, "gopherdex.test", "http://gopherdex.test")

	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})
	resp, body := alice.get("/account/security")
	expect(t, resp, body, http.StatusOK, "Passkeys")
	expect(t, resp, body, http.StatusOK, "data-passkey-register")

	// Add a passkey.
	resp, body = alice.postJSONAs("/account/passkeys/options", []byte(`{"password":"wrong"}`))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("options with a wrong password: %d %s", resp.StatusCode, body)
	}
	resp, body = alice.postJSONAs("/account/passkeys/options", []byte(`{"password":"correct horse battery"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("options: %d %s", resp.StatusCode, body)
	}
	var creation protocol.CredentialCreation
	if err := json.Unmarshal([]byte(body), &creation); err != nil {
		t.Fatal(err)
	}
	resp, body = alice.postJSONAs("/account/passkeys?name=Work+laptop", key.Create(t, &creation))
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"recoveryCodes":[`) {
		t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	_, body = alice.get("/account/security?done=passkey-added")
	if !strings.Contains(body, "<strong>Work laptop</strong>") || !strings.Contains(body, "1 active") {
		i := strings.Index(body, "pk-title")
		t.Errorf("security page doesn't list the passkey:\n%.1500s", body[max(0, i):])
	}
	env.mails.find(t, "alice@example.com", "A passkey was added")

	// Sign in with it, no password.
	fresh := newBrowser(t, env.srv.URL)
	_, body = fresh.postJSONAs("/login/passkey/options", nil)
	var assertion protocol.CredentialAssertion
	json.Unmarshal([]byte(body), &assertion)
	resp, body = fresh.postJSONAs("/login/passkey?next=/alice/retry", key.Get(t, &assertion))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"redirect":"/alice/retry"`) {
		t.Fatalf("passkey sign-in: %d %s", resp.StatusCode, body)
	}
	resp, body = fresh.get("/account")
	expect(t, resp, body, http.StatusOK, "@alice")

	// A second answer to the same ceremony is refused (its cookie is spent).
	resp, _ = fresh.postJSONAs("/login/passkey", key.Get(t, &assertion))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("replayed sign-in: %d", resp.StatusCode)
	}

	// A password sign-in now asks for the passkey.
	second := newBrowser(t, env.srv.URL)
	resp, _ = second.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login/2fa") {
		t.Fatalf("password sign-in went to %q", loc)
	}
	resp, body = second.get("/login/2fa")
	expect(t, resp, body, http.StatusOK, "Use a passkey")
	expect(t, resp, body, http.StatusOK, "Recovery code")
	_, body = second.postJSONAs("/login/2fa/passkey/options", nil)
	json.Unmarshal([]byte(body), &assertion)
	resp, body = second.postJSONAs("/login/2fa/passkey", key.Get(t, &assertion))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second factor: %d %s", resp.StatusCode, body)
	}
	resp, _ = second.get("/account")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after the second factor: %d", resp.StatusCode)
	}

	// Remove it (needs the password).
	_, body = alice.get("/account/security")
	id := regexp.MustCompile(`/account/passkeys/(\d+)/delete`).FindStringSubmatch(body)[1]
	resp, body = alice.post("/account/passkeys/"+id+"/delete", url.Values{"password": {"nope"}})
	expect(t, resp, body, http.StatusUnprocessableEntity, "wasn")
	resp, _ = alice.post("/account/passkeys/"+id+"/delete", url.Values{"password": {"correct horse battery"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("remove: %d", resp.StatusCode)
	}
	resp, body = alice.get("/account/security?done=passkey-removed")
	expect(t, resp, body, http.StatusOK, "Passkey removed.")

	// Signed-out scripts get a clear 401, not a redirect page.
	resp, _ = newBrowser(t, env.srv.URL).postJSONAs("/account/passkeys/options", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("options while signed out: %d", resp.StatusCode)
	}
}
