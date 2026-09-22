package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
)

func TestTeamsOverHTTP(t *testing.T) {
	env := newTestEnv(t)
	env.publish(t, "gopherdex.test/alice/retry", "v1.0.0") // creates alice
	for _, name := range []string{"bob", "carol"} {
		if _, err := env.accts.Register(context.Background(), name, name+"@example.com", "correct horse battery", accounts.Client{}); err != nil {
			t.Fatal(err)
		}
	}
	login := func(name string) *browser {
		b := newBrowser(t, env.srv.URL)
		resp, body := b.post("/login", url.Values{"login": {name}, "password": {"correct horse battery"}})
		expect(t, resp, body, http.StatusSeeOther, "")
		return b
	}
	alice, bob, carol, anon := login("alice"), login("bob"), login("carol"), newBrowser(t, env.srv.URL)

	resp, body := alice.post("/-/orgs", url.Values{"org": {"acme"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	alice.post("/-/orgs/members", url.Values{"org": {"acme"}, "username": {"bob"}, "role": {"member"}})
	resp, body = bob.post("/account/invitations/accept", url.Values{"kind": {"organization"}, "name": {"acme"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	env.publish(t, "gopherdex.test/acme/api", "v1.0.0")

	// Create a team; it opens on its own page.
	resp, body = bob.post("/-/orgs/teams", url.Values{"org": {"acme"}, "team": {"backend"}})
	expect(t, resp, body, http.StatusForbidden, "Only owners of acme")
	resp, body = alice.post("/-/orgs/teams", url.Values{"org": {"acme"}, "team": {"backend"}, "description": {"Runs the API"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if loc := resp.Header.Get("Location"); loc != "/orgs/acme/teams/backend?done=team-created" {
		t.Fatalf("after create: Location %q", loc)
	}
	resp, body = alice.post("/-/orgs/teams", url.Values{"org": {"acme"}, "team": {"Back End"}})
	expect(t, resp, body, http.StatusBadRequest, "lower-case letters")

	resp, body = alice.post("/-/orgs/teams/members", url.Values{"org": {"acme"}, "team": {"backend"}, "username": {"bob"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = alice.post("/-/orgs/teams/members", url.Values{"org": {"acme"}, "team": {"backend"}, "username": {"carol"}})
	expect(t, resp, body, http.StatusConflict, "Add them to the organization first")
	resp, body = alice.post("/-/orgs/teams/modules", url.Values{"org": {"acme"}, "team": {"backend"}, "module": {"gopherdex.test/acme/api"}, "role": {"owner"}})
	expect(t, resp, body, http.StatusSeeOther, "")

	// Members see teams; everyone else doesn't.
	resp, body = alice.get("/orgs/acme/teams/backend")
	expect(t, resp, body, http.StatusOK, "Runs the API")
	if !strings.Contains(body, "@bob") || !strings.Contains(body, "gopherdex.test/acme/api") || !strings.Contains(body, "Delete @acme/backend") {
		t.Error("team page should list members and modules, and let owners manage it")
	}
	resp, body = bob.get("/orgs/acme/teams/backend")
	expect(t, resp, body, http.StatusOK, "@acme/backend")
	if strings.Contains(body, "Delete @acme/backend") || strings.Contains(body, "Add to team") {
		t.Error("members must not see team management")
	}
	resp, body = bob.get("/acme")
	expect(t, resp, body, http.StatusOK, `href="/orgs/acme/teams/backend"`)
	if strings.Contains(body, "Save access") {
		t.Error("members must not see the access setting")
	}
	for _, b := range []*browser{anon, carol} {
		resp, body = b.get("/orgs/acme/teams/backend")
		expect(t, resp, body, http.StatusNotFound, "")
		resp, body = b.get("/acme")
		expect(t, resp, body, http.StatusOK, "")
		if strings.Contains(body, "/orgs/acme/teams/") {
			t.Error("teams shown to someone outside the organization")
		}
	}
	resp, body = alice.get("/orgs/acme/teams/nope")
	expect(t, resp, body, http.StatusNotFound, "")

	// The team makes bob an owner of the module.
	resp, body = bob.get("/acme/api?tab=manage")
	expect(t, resp, body, http.StatusOK, "Mark deprecated")
	if !strings.Contains(body, `href="/orgs/acme/teams/backend"`) {
		t.Error("manage tab should list teams with access")
	}

	// With access only through teams, removing the team's access locks bob out.
	resp, body = alice.post("/-/orgs/access", url.Values{"org": {"acme"}, "access": {"none"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = alice.get("/acme")
	expect(t, resp, body, http.StatusOK, "their teams decide which existing ones they maintain")
	resp, body = alice.post("/-/orgs/teams/modules/remove", url.Values{"org": {"acme"}, "team": {"backend"}, "module": {"gopherdex.test/acme/api"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = bob.get("/acme/api?tab=manage")
	expect(t, resp, body, http.StatusNotFound, "")

	resp, body = alice.post("/-/orgs/teams/delete", url.Values{"org": {"acme"}, "team": {"backend"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = alice.get("/acme?done=team-deleted")
	expect(t, resp, body, http.StatusOK, "Team deleted.")
}
