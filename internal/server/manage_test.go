package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
)

func TestManageFlows(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0")
	env.publish(t, "gopherdex.test/alice/retry", "v1.1.0")
	if _, err := env.accts.Register(context.Background(), "bob", "bob@example.com", "correct horse battery", accounts.Client{}); err != nil {
		t.Fatal(err)
	}

	anon := newBrowser(t, env.srv.URL)
	resp, body := anon.get("/alice/retry?tab=manage")
	expect(t, resp, body, http.StatusNotFound, "")

	alice := newBrowser(t, env.srv.URL)
	resp, body = alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}, "next": {"/alice/retry"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = alice.get("/alice/retry")
	expect(t, resp, body, http.StatusOK, "Manage <span>owner</span>")
	resp, body = alice.get("/alice/retry?tab=manage")
	expect(t, resp, body, http.StatusOK, "Mark deprecated")

	// Yank from the website.
	resp, body = alice.post("/-/yank", url.Values{"module": {"gopherdex.test/alice/retry"}, "version": {"v1.1.0"}, "reason": {"Leaks goroutines."}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if loc := resp.Header.Get("Location"); loc != "/alice/retry?done=yanked&tab=manage" {
		t.Fatalf("after yank: Location %q", loc)
	}
	resp, body = anon.get("/api/proxy/gopherdex.test/alice/retry/@v/list")
	expect(t, resp, body, http.StatusOK, "")
	if body != "v1.0.0\n" {
		t.Fatalf("list after yank = %q", body)
	}
	resp, body = anon.get("/alice/retry@v1.1.0")
	expect(t, resp, body, http.StatusOK, "was yanked by its maintainers.")
	if !strings.Contains(body, "Leaks goroutines.") {
		t.Error("yank reason missing from the version page")
	}
	resp, body = anon.get("/alice/retry")
	expect(t, resp, body, http.StatusOK, "v1.0.0")

	// Deprecate with a successor.
	resp, body = alice.post("/-/deprecate", url.Values{"module": {"gopherdex.test/alice/retry"}, "message": {"Use v2."}, "successor": {"gopherdex.test/alice/retry/v2"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = anon.get("/alice/retry")
	expect(t, resp, body, http.StatusOK, "<strong>Deprecated.</strong> Use v2.")
	if !strings.Contains(body, `href="/alice/retry/v2"`) {
		t.Error("successor link missing")
	}

	// Co-owners: bob becomes a maintainer.
	resp, body = alice.post("/-/collaborators", url.Values{"module": {"gopherdex.test/alice/retry"}, "username": {"@bob"}, "role": {"maintainer"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = alice.post("/-/collaborators", url.Values{"module": {"gopherdex.test/alice/retry"}, "username": {"nobody"}, "role": {"owner"}})
	expect(t, resp, body, http.StatusNotFound, "There&#39;s no user @nobody")

	bob := newBrowser(t, env.srv.URL)
	bob.post("/login", url.Values{"login": {"bob"}, "password": {"correct horse battery"}})
	// Until bob accepts, the role gives him nothing.
	resp, body = bob.get("/alice/retry?tab=manage")
	expect(t, resp, body, http.StatusNotFound, "")
	resp, body = bob.get("/account")
	expect(t, resp, body, http.StatusOK, "invited you to be a <strong>maintainer</strong>")
	resp, body = bob.post("/account/invitations/accept", url.Values{"kind": {"module"}, "name": {"gopherdex.test/alice/retry"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = bob.get("/alice/retry?tab=manage")
	expect(t, resp, body, http.StatusOK, "a maintainer")
	if strings.Contains(body, "Mark deprecated") || strings.Contains(body, "Add or update") {
		t.Error("maintainers must not see owner-only tools")
	}
	resp, body = bob.post("/-/undeprecate", url.Values{"module": {"gopherdex.test/alice/retry"}})
	expect(t, resp, body, http.StatusForbidden, "You need to be an owner")

	// Organizations.
	resp, body = alice.post("/-/orgs", url.Values{"org": {"acme"}, "display_name": {"Acme Corp"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = alice.post("/-/orgs/members", url.Values{"org": {"acme"}, "username": {"bob"}, "role": {"member"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = anon.get("/acme")
	expect(t, resp, body, http.StatusOK, "Acme Corp")
	if !strings.Contains(body, "@bob") || strings.Contains(body, "Add or update") {
		t.Error("org page should list members and hide management from visitors")
	}
	resp, body = bob.post("/-/orgs/members/remove", url.Values{"org": {"acme"}, "username": {"alice"}})
	expect(t, resp, body, http.StatusForbidden, "Only owners of acme")
	resp, body = alice.post("/-/orgs", url.Values{"org": {"bob"}})
	expect(t, resp, body, http.StatusConflict, "is taken")
	resp, body = alice.get("/account")
	expect(t, resp, body, http.StatusOK, "gopherdex.test/acme/")

}
