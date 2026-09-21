package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
)

// sentTo reports whether any email to "to" has a subject containing subject.
func (m *mailRecorder) sentTo(to, subject string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.sent {
		if msg.To == to && strings.Contains(msg.Subject, subject) {
			return true
		}
	}
	return false
}

func TestAccountSettings(t *testing.T) {
	env := newTestEnv(t)
	mod := "gopherdex.test/alice/retry"
	env.publish(t, mod, "v1.0.0")
	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})

	// Change email: nothing changes until the new address confirms.
	resp, body := alice.post("/account/email", url.Values{"new_email": {"alice@new.example"}, "current_password": {"wrong"}})
	expect(t, resp, body, http.StatusUnprocessableEntity, "current password isn")
	resp, body = alice.post("/account/email", url.Values{"new_email": {"alice@new.example"}, "current_password": {"correct horse battery"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = alice.get("/account?done=email-change-sent")
	expect(t, resp, body, http.StatusOK, "Waiting for you to open the link sent to <strong>alice@new.example</strong>")
	link := regexp.MustCompile(`/verify-email\?token=\S+`).FindString(env.mails.find(t, "alice@new.example", "Confirm your new email"))
	resp, _ = alice.get(link)
	if loc := resp.Header.Get("Location"); loc != "/account?done=email-changed" {
		t.Errorf("after confirming, redirect to %q", loc)
	}
	env.mails.find(t, "alice@example.com", "email address was changed")
	_, body = alice.get("/account")
	if !strings.Contains(body, "<strong>alice@new.example</strong>") {
		t.Error("account page doesn't show the new address")
	}

	// Preferences: with publish emails off, publishing sends none.
	resp, body = alice.post("/account/preferences", url.Values{"access": {"on"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	env.publish(t, mod, "v1.1.0")
	time.Sleep(200 * time.Millisecond) // publish emails go out in the background
	if env.mails.sentTo("alice@new.example", mod+" v1.1.0 was published") {
		t.Error("publish email sent although turned off")
	}
	_, body = alice.get("/account")
	if !strings.Contains(body, `name="publish">`) || !strings.Contains(body, `name="access" checked>`) {
		t.Error("preferences not shown as saved")
	}

	// Access emails: bob hears when alice adds him.
	if _, err := env.accts.Register(context.Background(), "bob", "bob@example.com", "correct horse battery", accounts.Client{}); err != nil {
		t.Fatal(err)
	}
	resp, body = alice.post("/-/collaborators", url.Values{"module": {mod}, "username": {"@Bob"}, "role": {"maintainer"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if msg := env.mails.find(t, "bob@example.com", "You're now a maintainer of "+mod); !strings.Contains(msg, "@alice made you a maintainer") {
		t.Errorf("access email:\n%s", msg)
	}

	// Delete the account.
	resp, body = alice.post("/account/delete", url.Values{"delete_password": {"correct horse battery"}, "confirm": {"bob"}})
	expect(t, resp, body, http.StatusUnprocessableEntity, "Type your username, alice")
	resp, body = alice.post("/account/delete", url.Values{"delete_password": {"nope"}, "confirm": {"alice"}})
	expect(t, resp, body, http.StatusUnprocessableEntity, "That password isn")
	resp, body = alice.post("/account/delete", url.Values{"delete_password": {"correct horse battery"}, "confirm": {"@alice"}})
	expect(t, resp, body, http.StatusOK, "Your account is gone.")
	resp, _ = alice.get("/account")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("account page after deletion: %d, want a redirect to sign in", resp.StatusCode)
	}
	// The module stays, with bob still maintaining it.
	resp, body = fetch(t, env.srv, "/alice/retry")
	expect(t, resp, body, http.StatusOK, "v1.1.0")
	if role, _ := env.reg.Collaborators(context.Background(), mod); len(role) != 1 || role[0].Username != "bob" {
		t.Errorf("collaborators after deletion: %+v", role)
	}
}

func TestFeeds(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	env.publish(t, "gopherdex.test/alice/retry", "v1.1.0")
	env.publish(t, "gopherdex.test/alice/retry/v2", "v2.0.0")

	for path, want := range map[string][]string{
		"/feeds/releases.atom":       {`<feed xmlns="http://www.w3.org/2005/Atom">`, "<title>gopherdex.test/alice/retry v1.1.0</title>", "<title>gopherdex.test/alice/retry/v2 v2.0.0</title>", `href="http://gopherdex.test/alice/retry@v1.0.0"`, "<name>@alice</name>"},
		"/feeds/new.atom":            {"<title>gopherdex.test/alice/retry</title>", "<title>gopherdex.test/alice/retry/v2</title>"},
		"/feeds/alice.atom":          {"<title>Releases by @alice</title>", "v2.0.0"},
		"/feeds/alice/retry.atom":    {"<title>Releases of gopherdex.test/alice/retry</title>", "v1.0.0", "<summary>Package lib is a test.</summary>"},
		"/feeds/alice/retry/v2.atom": {"v2.0.0"},
	} {
		resp, body := fetch(t, env.srv, path)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/atom+xml; charset=utf-8" {
			t.Errorf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
			continue
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s doesn't contain %q:\n%s", path, w, body)
			}
		}
	}
	if _, body := fetch(t, env.srv, "/feeds/alice/retry.atom"); strings.Contains(body, "v2.0.0") {
		t.Error("the v1 module feed includes v2 releases")
	}
	for _, path := range []string{"/feeds/nobody.atom", "/feeds/alice/missing.atom", "/feeds/alice", "/feeds/.atom"} {
		if resp, _ := fetch(t, env.srv, path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}

	// Pages point feed readers at their feed.
	for page, feed := range map[string]string{"/": "/feeds/releases.atom", "/alice": "/feeds/alice.atom", "/alice/retry": "/feeds/alice/retry.atom"} {
		if _, body := fetch(t, env.srv, page); !strings.Contains(body, `<link rel="alternate" type="application/atom+xml"`) || !strings.Contains(body, `href="`+feed+`"`) {
			t.Errorf("%s doesn't advertise %s", page, feed)
		}
	}
}
