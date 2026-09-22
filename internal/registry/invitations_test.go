package registry

import (
	"context"
	"net/http"
	"testing"
)

func TestInvitations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	victim, victimTok := f.user(t, "victim")

	// The attack: make someone else the owner of your organization, then
	// leave them holding it. A pending invitation doesn't count.
	if err := f.reg.CreateOrg(ctx, f.alice, "evil", "", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.SetOrgMember(ctx, f.alice, "evil", "victim", "owner", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.RemoveOrgMember(ctx, f.alice, "evil", "alice", client); !isReject(err, http.StatusConflict, "last_owner") {
		t.Fatalf("leaving a pending invitee as the only owner: %v", err)
	}
	if ok, _ := f.reg.IsOrgOwner(ctx, victim, "evil"); ok {
		t.Fatal("an invitation made them an owner")
	}
	if err := f.publishAs(t, victim, victimTok, host+"/evil/x", "v1.0.0"); !isReject(err, http.StatusForbidden, "forbidden_namespace") {
		t.Fatalf("invitee publishing: %v", err)
	}
	members, _ := f.reg.OrgMembers(ctx, "evil")
	if len(members) != 2 || !members[1].Pending || members[1].Username != "victim" {
		t.Fatalf("members = %+v", members)
	}
	if ms, _ := f.reg.Memberships(ctx, victim.ID); len(ms) != 0 {
		t.Fatalf("memberships before accepting = %+v", ms)
	}

	// Same for a module.
	mod := host + "/alice/retry"
	if err := f.publishAs(t, f.alice, f.token, mod, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	f.reg.SetCollaborator(ctx, f.alice, mod, "victim", "owner", client)
	if err := f.reg.RemoveCollaborator(ctx, f.alice, mod, "alice", client); !isReject(err, http.StatusConflict, "last_owner") {
		t.Fatalf("leaving a pending invitee as the module's only owner: %v", err)
	}
	if role, _ := f.reg.Role(ctx, victim, mod); role != RoleNone {
		t.Fatalf("pending role = %v", role)
	}

	// The invitee sees both, and decides.
	invs, err := f.reg.Invitations(ctx, victim)
	if err != nil || len(invs) != 2 || invs[0].InvitedBy != "alice" {
		t.Fatalf("invitations = %+v, %v", invs, err)
	}
	if pending, _ := f.reg.InvitationPending(ctx, InviteOrg, "evil", "victim"); !pending {
		t.Error("InvitationPending")
	}
	if err := f.reg.DeclineInvitation(ctx, victim, InviteOrg, "evil", client); err != nil {
		t.Fatal(err)
	}
	if members, _ := f.reg.OrgMembers(ctx, "evil"); len(members) != 1 {
		t.Fatalf("declined invitee still listed: %+v", members)
	}
	if err := f.reg.AcceptInvitation(ctx, victim, InviteModule, mod, client); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.reg.Role(ctx, victim, mod); role != RoleOwner {
		t.Fatalf("accepted role = %v", role)
	}
	if err := f.reg.AcceptInvitation(ctx, victim, InviteModule, mod, client); !isReject(err, http.StatusNotFound, "no_invitation") {
		t.Fatalf("accepting twice: %v", err)
	}
	// Now there are two owners, either can leave; the last can't.
	if err := f.reg.LeaveModule(ctx, f.alice, mod, client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.LeaveModule(ctx, victim, mod, client); !isReject(err, http.StatusConflict, "last_owner") {
		t.Fatalf("last owner leaving: %v", err)
	}

	// Leaving an organization.
	f.reg.SetOrgMember(ctx, f.alice, "evil", "victim", "member", client)
	f.reg.AcceptInvitation(ctx, victim, InviteOrg, "evil", client)
	if err := f.reg.LeaveOrg(ctx, victim, "evil", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.LeaveOrg(ctx, f.alice, "evil", client); !isReject(err, http.StatusConflict, "last_owner") {
		t.Fatalf("last org owner leaving: %v", err)
	}
}
