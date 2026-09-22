package registry

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

func TestTeams(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bob, bobTok := f.user(t, "bob")
	carol, carolTok := f.user(t, "carol")
	dave, _ := f.user(t, "dave")

	if err := f.reg.CreateOrg(ctx, f.alice, "acme", "", client); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bob", "carol"} {
		if err := f.setOrgMember(ctx, f.alice, "acme", name, "member", client); err != nil {
			t.Fatal(err)
		}
	}
	api, web := host+"/acme/api", host+"/acme/web"
	if err := f.publishAs(t, f.alice, f.token, api, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := f.publishAs(t, f.alice, f.token, web, "v1.0.0"); err != nil {
		t.Fatal(err)
	}

	// Only org owners manage teams; names like "security" are fine.
	if err := f.reg.CreateTeam(ctx, bob, "acme", "backend", "", client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Fatalf("member creating a team: %v", err)
	}
	for _, bad := range []string{"", "-x", "Back End", "a--b"} {
		if err := f.reg.CreateTeam(ctx, f.alice, "acme", bad, "", client); !isReject(err, http.StatusBadRequest, "invalid_team_name") {
			t.Errorf("team name %q: %v", bad, err)
		}
	}
	if err := f.reg.CreateTeam(ctx, f.alice, "acme", "Backend", "Owns the API", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.CreateTeam(ctx, f.alice, "acme", "backend", "", client); !isReject(err, http.StatusConflict, "team_exists") {
		t.Fatalf("duplicate team: %v", err)
	}
	if err := f.reg.CreateTeam(ctx, f.alice, "acme", "security", "", client); err != nil {
		t.Fatal(err)
	}

	// Members join; outsiders must join the organization first.
	if err := f.reg.AddTeamMember(ctx, f.alice, "acme", "backend", "dave", client); !isReject(err, http.StatusConflict, "not_an_org_member") {
		t.Fatalf("outsider in a team: %v", err)
	}
	if err := f.reg.AddTeamMember(ctx, f.alice, "acme", "backend", "@bob", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.SetTeamModule(ctx, f.alice, "acme", "backend", "api", "owner", client); err != nil { // short name
		t.Fatal(err)
	}
	if err := f.reg.SetTeamModule(ctx, f.alice, "acme", "backend", host+"/alice/other", "maintainer", client); !isReject(err, http.StatusNotFound, "unknown_module") {
		t.Fatalf("unknown module: %v", err)
	}
	if err := f.publishAs(t, f.alice, f.token, host+"/alice/mine", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.SetTeamModule(ctx, f.alice, "acme", "backend", host+"/alice/mine", "maintainer", client); !isReject(err, http.StatusForbidden, "other_namespace") {
		t.Fatalf("module outside the org: %v", err)
	}

	// With the default member access, the team adds owner on top of
	// everyone maintaining everything.
	if role, _ := f.reg.Role(ctx, bob, api); role != RoleOwner {
		t.Fatalf("team owner role = %v", role)
	}
	if role, _ := f.reg.Role(ctx, carol, api); role != RoleMaintainer {
		t.Fatalf("member without a team = %v", role)
	}

	// With member access "none", only teams (and direct roles) count.
	if err := f.reg.SetMemberAccess(ctx, bob, "acme", MemberAccessNone, client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Fatalf("member changing access: %v", err)
	}
	if err := f.reg.SetMemberAccess(ctx, f.alice, "acme", MemberAccessNone, client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, carol, api); role != RoleNone {
		t.Fatalf("member without a team, access none = %v", role)
	}
	if err := f.publishAs(t, carol, carolTok, api, "v1.1.0"); !isReject(err, http.StatusForbidden, "forbidden_module") {
		t.Fatalf("member without a team publishing: %v", err)
	}
	if role, _ := f.reg.Role(ctx, bob, web); role != RoleNone {
		t.Fatalf("team member on a module the team doesn't cover = %v", role)
	}
	if err := f.publishAs(t, bob, bobTok, api, "v1.1.0"); err != nil {
		t.Fatalf("team member publishing: %v", err)
	}
	if role, _ := f.reg.Role(ctx, f.alice, web); role != RoleOwner {
		t.Fatalf("org owner = %v", role)
	}
	// A member who creates a module maintains it.
	if err := f.publishAs(t, carol, carolTok, host+"/acme/cli", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, carol, host+"/acme/cli"); role != RoleMaintainer {
		t.Fatalf("creator of an org module = %v", role)
	}
	if role, _ := f.reg.Role(ctx, bob, host+"/acme/cli"); role != RoleNone {
		t.Fatalf("other member on a new module = %v", role)
	}
	// Notifications go to owners, the team and direct collaborators only.
	if got, _ := f.reg.Maintainers(ctx, api); !slices.Equal(got, []string{"alice", "bob"}) {
		t.Fatalf("maintainers of api = %v", got)
	}

	// Listings.
	teams, _ := f.reg.Teams(ctx, "acme")
	if len(teams) != 2 || teams[0].Name != "backend" || teams[0].Members != 1 || teams[0].Modules != 1 || teams[0].Description != "Owns the API" {
		t.Fatalf("teams = %+v", teams)
	}
	if got, _ := f.reg.ModuleTeams(ctx, api); len(got) != 1 || got[0] != (TeamAccess{"backend", "owner"}) {
		t.Fatalf("module teams = %+v", got)
	}
	if ok, _ := f.reg.IsOrgMember(ctx, dave, "acme"); ok {
		t.Fatal("dave isn't a member")
	}

	// Downgrading and removing access.
	if err := f.reg.SetTeamModule(ctx, f.alice, "acme", "backend", api, "maintainer", client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, bob, api); role != RoleMaintainer {
		t.Fatalf("after downgrade = %v", role)
	}
	if err := f.reg.RemoveTeamModule(ctx, f.alice, "acme", "backend", api, client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, bob, api); role != RoleNone {
		t.Fatalf("after removing team access = %v", role)
	}

	// Leaving the organization leaves its teams.
	f.reg.SetTeamModule(ctx, f.alice, "acme", "backend", api, "owner", client)
	if err := f.reg.RemoveOrgMember(ctx, f.alice, "acme", "bob", client); err != nil {
		t.Fatal(err)
	}
	if members, _ := f.reg.TeamMembers(ctx, teams[0].ID); len(members) != 0 {
		t.Fatalf("team members after leaving the org = %+v", members)
	}
	if role, _ := f.reg.Role(ctx, bob, api); role != RoleNone {
		t.Fatalf("ex-member role = %v", role)
	}

	// Deleting a team takes its access away.
	f.setOrgMember(ctx, f.alice, "acme", "bob", "member", client)
	f.reg.AddTeamMember(ctx, f.alice, "acme", "backend", "bob", client)
	if role, _ := f.reg.Role(ctx, bob, api); role != RoleOwner {
		t.Fatalf("rejoined = %v", role)
	}
	if err := f.reg.DeleteTeam(ctx, f.alice, "acme", "backend", client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, bob, api); role != RoleNone {
		t.Fatalf("after deleting the team = %v", role)
	}
	if _, err := f.reg.TeamByName(ctx, "acme", "backend"); !isReject(err, http.StatusNotFound, "unknown_team") {
		t.Fatalf("deleted team: %v", err)
	}
}

func TestSecurityReviewAuthorization(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bob, bobTok := f.user(t, "bob")
	carol, carolTok := f.user(t, "carol")
	f.reg.CreateOrg(ctx, f.alice, "acme", "", client)
	f.setOrgMember(ctx, f.alice, "acme", "bob", "member", client)
	f.setOrgMember(ctx, f.alice, "acme", "carol", "member", client)
	f.reg.SetMemberAccess(ctx, f.alice, "acme", MemberAccessNone, client)

	// A member who created a module loses it when removed from the org.
	lib := host + "/acme/newlib"
	if err := f.publishAs(t, bob, bobTok, lib, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.RemoveOrgMember(ctx, f.alice, "acme", "bob", client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, bob, lib); role != RoleNone {
		t.Fatalf("removed member keeps %v on the module they created", role)
	}
	if err := f.publishAs(t, bob, bobTok, lib, "v1.1.0"); !isReject(err, http.StatusForbidden, "forbidden_module") {
		t.Fatalf("removed member publishing: %v", err)
	}
	// Outside collaborators (never members) keep their direct role.
	if err := f.setCollaborator(ctx, f.alice, lib, "bob", "maintainer", client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, bob, lib); role != RoleMaintainer {
		t.Fatalf("outside collaborator: %v", role)
	}

	// A new major version needs rights on the module it continues.
	api := host + "/acme/api"
	if err := f.publishAs(t, f.alice, f.token, api, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := f.publishAs(t, carol, carolTok, api+"/v2", "v2.0.0"); !isReject(err, http.StatusForbidden, "forbidden_module") {
		t.Fatalf("member without access publishing a new major: %v", err)
	}
	if err := f.publishAs(t, f.alice, f.token, api+"/v2", "v2.0.0"); err != nil {
		t.Fatalf("owner publishing a new major: %v", err)
	}

	// Quarantined majors aren't linked from the others.
	if err := f.reg.Quarantine(ctx, f.alice, api+"/v2", "review", client); err != nil {
		t.Fatal(err)
	}
	if majors, _ := f.reg.MajorVersions(ctx, api); len(majors) != 1 || majors[0] != api {
		t.Fatalf("majors = %v", majors)
	}
	_ = carol
}
