package registry

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/codebled/gopherdex/internal/accounts"
	"github.com/codebled/gopherdex/internal/module"
)

var client = accounts.Client{IP: "192.0.2.1"}

func (f *fixture) publishAs(t *testing.T, u *accounts.User, tok *accounts.Token, modPath, version string) error {
	t.Helper()
	up := f.upload(modPath, version, f.zipModule(t, modPath, version, retryFiles(modPath, " "+version)))
	up.User, up.Token = u, tok
	_, err := f.reg.Publish(context.Background(), up)
	return err
}

func TestYank(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	for _, v := range []string{"v1.0.0", "v1.1.0"} {
		if err := f.publishAs(t, f.alice, f.token, mod, v); err != nil {
			t.Fatal(err)
		}
	}
	bob, _ := f.user(t, "bob")
	if err := f.reg.Yank(ctx, bob, mod, "v1.1.0", "", client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Fatalf("stranger yank: %v", err)
	}
	if err := f.reg.Yank(ctx, f.alice, mod, "v1.1.0", "Deadlocks under load.", client); err != nil {
		t.Fatal(err)
	}

	// The go command no longer sees it in lists or @latest…
	versions, _ := f.reg.Versions(ctx, mod)
	if !slices.Equal(versions, []string{"v1.0.0"}) {
		t.Fatalf("list after yank = %v", versions)
	}
	if info, err := f.reg.Latest(ctx, mod); err != nil || info.Version != "v1.0.0" {
		t.Fatalf("@latest after yank = %+v, %v", info, err)
	}
	// …but pinned builds can still download it.
	if _, err := f.reg.GoMod(ctx, mod, "v1.1.0"); err != nil {
		t.Fatalf("yanked go.mod must stay downloadable: %v", err)
	}
	if _, err := f.reg.Zip(ctx, mod, "v1.1.0"); err != nil {
		t.Fatalf("yanked zip must stay downloadable: %v", err)
	}
	m, _ := f.reg.Module(ctx, mod)
	if !m.Versions[0].Yanked || m.Versions[0].YankReason != "Deadlocks under load." {
		t.Fatalf("detail = %+v", m.Versions[0])
	}
	listed, _ := f.reg.RecentlyUpdated(ctx, 10, 0)
	if len(listed) != 1 || listed[0].Version != "v1.0.0" {
		t.Fatalf("listings must skip yanked versions: %+v", listed)
	}

	// Yanking everything leaves an existing module with nothing installable.
	f.reg.Yank(ctx, f.alice, mod, "v1.0.0", "", client)
	if versions, err := f.reg.Versions(ctx, mod); err != nil || len(versions) != 0 {
		t.Fatalf("all yanked: %v, %v", versions, err)
	}
	if _, err := f.reg.Latest(ctx, mod); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("all yanked @latest: %v", err)
	}
	if root, err := f.reg.ModuleRoot(ctx, mod); err != nil || root != mod {
		t.Fatalf("go-import must keep working for pinned users: %q, %v", root, err)
	}

	if err := f.reg.Unyank(ctx, f.alice, mod, "v1.1.0", client); err != nil {
		t.Fatal(err)
	}
	if info, _ := f.reg.Latest(ctx, mod); info.Version != "v1.1.0" {
		t.Fatalf("@latest after unyank = %s", info.Version)
	}
	if err := f.reg.Yank(ctx, f.alice, mod, "v9.0.0", "", client); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("yank unknown version: %v", err)
	}
}

func TestCollaboratorsAndTransfer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	if err := f.publishAs(t, f.alice, f.token, mod, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	bob, bobTok := f.user(t, "bob")

	// Bob can't publish alice's module…
	if err := f.publishAs(t, bob, bobTok, mod, "v1.1.0"); !isReject(err, http.StatusForbidden, "forbidden_module") {
		t.Fatalf("bob before invite: %v", err)
	}
	// …until alice makes him a maintainer. Maintainers publish and yank,
	// but can't manage access or deprecate.
	if err := f.setCollaborator(ctx, f.alice, mod, "@Bob", "maintainer", client); err != nil {
		t.Fatal(err)
	}
	if err := f.publishAs(t, bob, bobTok, mod, "v1.1.0"); err != nil {
		t.Fatalf("maintainer publish: %v", err)
	}
	if err := f.reg.Yank(ctx, bob, mod, "v1.1.0", "", client); err != nil {
		t.Fatalf("maintainer yank: %v", err)
	}
	if err := f.reg.Deprecate(ctx, bob, mod, "old", "", client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Fatalf("maintainer deprecate: %v", err)
	}
	if err := f.setCollaborator(ctx, bob, mod, "bob", "owner", client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Fatalf("maintainer self-promotion: %v", err)
	}

	// Alice can't leave the module without an owner.
	if err := f.reg.RemoveCollaborator(ctx, f.alice, mod, "alice", client); !isReject(err, http.StatusConflict, "last_owner") {
		t.Fatalf("remove last owner: %v", err)
	}

	// Transfer: make bob an owner, then alice removes herself.
	if err := f.setCollaborator(ctx, f.alice, mod, "bob", "owner", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.RemoveCollaborator(ctx, f.alice, mod, "alice", client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, f.alice, mod); role != RoleNone {
		t.Fatalf("alice after transfer: %v", role)
	}
	if err := f.publishAs(t, f.alice, f.token, mod, "v1.2.0"); !isReject(err, http.StatusForbidden, "forbidden_module") {
		t.Fatalf("alice publishing after transfer: %v", err)
	}
	collabs, _ := f.reg.Collaborators(ctx, mod)
	if len(collabs) != 1 || collabs[0].Username != "bob" || collabs[0].Role != "owner" {
		t.Fatalf("collaborators = %+v", collabs)
	}
	if err := f.setCollaborator(ctx, bob, mod, "nobody", "maintainer", client); !isReject(err, http.StatusNotFound, "unknown_user") {
		t.Fatalf("unknown user: %v", err)
	}
	managed, _ := f.reg.Managed(ctx, bob)
	if len(managed) != 1 || managed[0].Path != mod {
		t.Fatalf("bob manages %+v", managed)
	}
}

func TestDeprecate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	f.publishAs(t, f.alice, f.token, mod, "v1.0.0")

	if err := f.reg.Deprecate(ctx, f.alice, mod, "  ", "", client); !isReject(err, http.StatusBadRequest, "missing_message") {
		t.Fatalf("empty message: %v", err)
	}
	if err := f.reg.Deprecate(ctx, f.alice, mod, "Moved.", mod, client); !isReject(err, http.StatusBadRequest, "invalid_successor") {
		t.Fatalf("self successor: %v", err)
	}
	if err := f.reg.Deprecate(ctx, f.alice, mod, "Moved to v2.", mod+"/v2", client); err != nil {
		t.Fatal(err)
	}
	m, _ := f.reg.Module(ctx, mod)
	if m.Deprecation != "Moved to v2." || m.Successor != mod+"/v2" {
		t.Fatalf("module = %+v", m)
	}
	f.reg.Undeprecate(ctx, f.alice, mod, client)
	if m, _ := f.reg.Module(ctx, mod); m.Deprecation != "" || m.Successor != "" {
		t.Fatalf("after undeprecate = %+v", m)
	}
}

func TestOrganizations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bob, bobTok := f.user(t, "bob")
	carol, carolTok := f.user(t, "carol")

	if err := f.reg.CreateOrg(ctx, f.alice, "acme", "Acme Corp", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.CreateOrg(ctx, bob, "acme", "", client); !isReject(err, http.StatusConflict, "name_taken") {
		t.Fatalf("duplicate org: %v", err)
	}
	if err := f.reg.CreateOrg(ctx, bob, "alice", "", client); !isReject(err, http.StatusConflict, "name_taken") {
		t.Fatalf("org named like a user: %v", err)
	}
	if err := f.reg.CreateOrg(ctx, bob, "api", "", client); !isReject(err, http.StatusBadRequest, "invalid_name") {
		t.Fatalf("reserved org name: %v", err)
	}

	mod := host + "/acme/widgets"
	if err := f.publishAs(t, bob, bobTok, mod, "v1.0.0"); !isReject(err, http.StatusForbidden, "forbidden_namespace") {
		t.Fatalf("non-member publish: %v", err)
	}
	if err := f.setOrgMember(ctx, bob, "acme", "carol", "member", client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Fatalf("non-owner adding members: %v", err)
	}
	if err := f.setOrgMember(ctx, f.alice, "acme", "bob", "member", client); err != nil {
		t.Fatal(err)
	}
	if err := f.publishAs(t, bob, bobTok, mod, "v1.0.0"); err != nil {
		t.Fatalf("member publish: %v", err)
	}
	// Org owners own every module in the namespace; members maintain them.
	if role, _ := f.reg.Role(ctx, f.alice, mod); role != RoleOwner {
		t.Fatalf("org owner role = %v", role)
	}
	if role, _ := f.reg.Role(ctx, bob, mod); role != RoleMaintainer {
		t.Fatalf("org member role = %v", role)
	}
	if err := f.publishAs(t, carol, carolTok, mod, "v1.1.0"); !isReject(err, http.StatusForbidden, "forbidden_module") {
		t.Fatalf("outsider publish to org module: %v", err)
	}

	if err := f.reg.RemoveOrgMember(ctx, f.alice, "acme", "alice", client); !isReject(err, http.StatusConflict, "last_owner") {
		t.Fatalf("remove last org owner: %v", err)
	}
	if err := f.reg.RemoveOrgMember(ctx, f.alice, "acme", "bob", client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, bob, mod); role != RoleNone {
		t.Fatalf("removed member keeps role %v", role)
	}
	members, _ := f.reg.OrgMembers(ctx, "acme")
	if len(members) != 1 || members[0].Username != "alice" {
		t.Fatalf("members = %+v", members)
	}
	ms, _ := f.reg.Memberships(ctx, f.alice.ID)
	if len(ms) != 1 || ms[0].Org != "acme" || ms[0].Role != "owner" {
		t.Fatalf("memberships = %+v", ms)
	}
}
