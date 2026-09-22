package accounts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// waitForMail waits for a background email to "to" whose subject contains
// subject.
func (r *recorder) waitForMail(t *testing.T, to, subject string) string {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		for _, m := range r.sent {
			if m.To == to && strings.Contains(m.Subject, subject) {
				r.mu.Unlock()
				return m.Body
			}
		}
		r.mu.Unlock()
	}
	t.Fatalf("no email to %s about %q", to, subject)
	return ""
}

func verifiedUser(t *testing.T, s *Service, rec *recorder, name string) *User {
	t.Helper()
	u, err := s.Register(context.Background(), name, name+"@example.com", "correct horse battery", client)
	if err != nil {
		t.Fatal(err)
	}
	if u, err = s.VerifyEmail(context.Background(), rec.linkToken(t, u.Email), client); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestChangeEmail(t *testing.T) {
	s, rec, _ := newService(t)
	ctx := context.Background()
	alice := verifiedUser(t, s, rec, "alice")
	verifiedUser(t, s, rec, "bob")

	for name, tc := range map[string]struct{ email, password, field string }{
		"same address":   {"alice@example.com", "correct horse battery", "email"},
		"taken":          {"BOB@example.com", "correct horse battery", "email"},
		"invalid":        {"not-an-email", "correct horse battery", "email"},
		"wrong password": {"alice@new.example", "nope", "current_password"},
	} {
		var fe *FieldError
		if err := s.RequestEmailChange(ctx, alice, tc.email, tc.password, client); !errors.As(err, &fe) || fe.Field != tc.field {
			t.Errorf("%s: err = %v, want a %s field error", name, err, tc.field)
		}
	}

	if err := s.RequestEmailChange(ctx, alice, "Alice@New.Example", "correct horse battery", client); err != nil {
		t.Fatal(err)
	}
	rec.waitForMail(t, "alice@example.com", "Email change requested")
	if p, _ := s.PendingEmail(ctx, alice); p != "alice@new.example" {
		t.Errorf("pending = %q", p)
	}
	if u, _ := s.userByID(ctx, alice.ID); u.Email != "alice@example.com" {
		t.Errorf("email changed before confirmation: %s", u.Email)
	}

	u, err := s.VerifyEmail(ctx, rec.linkToken(t, "alice@new.example"), client)
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "alice@new.example" || !u.EmailVerified {
		t.Errorf("after confirming: %+v", u)
	}
	body := rec.waitForMail(t, "alice@example.com", "email address was changed")
	if !strings.Contains(body, "alice@new.example") {
		t.Errorf("old-address notice:\n%s", body)
	}
	if p, _ := s.PendingEmail(ctx, u); p != "" {
		t.Errorf("still pending: %q", p)
	}
	// Password reset now goes to the new address.
	if err := s.RequestPasswordReset(ctx, "alice@new.example", client); err != nil {
		t.Fatal(err)
	}
	rec.waitForMail(t, "alice@new.example", "Reset")

	// Someone else takes the address while a change waits: the link fails.
	if err := s.RequestEmailChange(ctx, u, "carol@example.com", "correct horse battery", client); err != nil {
		t.Fatal(err)
	}
	link := rec.linkToken(t, "carol@example.com")
	if _, err := s.Register(ctx, "carol", "carol@example.com", "correct horse battery", client); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyEmail(ctx, link, client); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("confirming a taken address: %v", err)
	}
}

func TestPreferences(t *testing.T) {
	s, rec, _ := newService(t)
	ctx := context.Background()
	alice := verifiedUser(t, s, rec, "alice")
	bob := verifiedUser(t, s, rec, "bob")

	if p, err := s.Preferences(ctx, alice.ID); err != nil || !p.Publish || !p.Access {
		t.Fatalf("defaults = %+v, %v; want everything on", p, err)
	}
	if err := s.SetPreferences(ctx, bob, Preferences{Publish: false, Access: true}, client); err != nil {
		t.Fatal(err)
	}
	got, err := s.EmailsFor(ctx, []string{"alice", "bob", "nobody"}, EmailPublish)
	if err != nil || len(got) != 1 || got["alice"] != "alice@example.com" {
		t.Errorf("publish emails = %v, %v", got, err)
	}
	if got, _ := s.EmailsFor(ctx, []string{"alice", "bob"}, EmailAccess); len(got) != 2 {
		t.Errorf("access emails = %v", got)
	}
	// Security emails ignore preferences.
	if got, _ := s.UserEmails(ctx, []string{"bob"}); got["bob"] == "" {
		t.Error("UserEmails skipped bob")
	}
}

func TestDeleteAccount(t *testing.T) {
	s, rec, c := newService(t)
	ctx := context.Background()
	alice := verifiedUser(t, s, rec, "alice")
	session, err := s.StartSession(ctx, alice, client)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.CreateToken(ctx, alice, "ci", 0, "", client)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAccount(ctx, alice, "wrong", "", client); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}

	// The only owner of an organization can't leave it ownerless.
	db := s.DB
	db.ExecContext(ctx, `INSERT INTO namespaces (name, kind, created_at) VALUES ('acme', 'org', 1)`)
	db.ExecContext(ctx, `INSERT INTO organizations (id, name, display_name, created_by, created_at) VALUES (1, 'acme', 'Acme', ?, 1)`, alice.ID)
	if _, err := db.ExecContext(ctx, `INSERT INTO org_members (org_id, user_id, role, created_at, accepted_at) VALUES (1, ?, 'owner', 1, 1)`, alice.ID); err != nil {
		t.Fatal(err)
	}
	var last *ErrLastOrgOwner
	if err := s.DeleteAccount(ctx, alice, "correct horse battery", "", client); !errors.As(err, &last) || last.Orgs[0] != "@acme" {
		t.Fatalf("last org owner: %v", err)
	}
	db.ExecContext(ctx, `DELETE FROM org_members`)

	// With two-factor on, a code is needed too.
	setup, err := s.BeginTOTP(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := TOTPCode(setup.Secret, c.t)
	if _, err := s.ConfirmTOTP(ctx, alice, code, client); err != nil {
		t.Fatal(err)
	}
	alice.TwoFactor = true
	if err := s.DeleteAccount(ctx, alice, "correct horse battery", "000000", client); !errors.Is(err, ErrBadSecondFactor) {
		t.Fatalf("wrong code: %v", err)
	}
	c.t = c.t.Add(time.Minute)
	code, _ = TOTPCode(setup.Secret, c.t)
	if err := s.DeleteAccount(ctx, alice, "correct horse battery", code, client); err != nil {
		t.Fatal(err)
	}

	rec.waitForMail(t, "alice@example.com", "account was deleted")
	if _, err := s.SessionUser(ctx, session); err == nil {
		t.Error("session survived deletion")
	}
	if _, _, err := s.AuthenticateToken(ctx, secret); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("token survived deletion: %v", err)
	}
	// The name stays reserved; the email address is free again.
	if _, err := s.Register(ctx, "alice", "someone@example.com", "correct horse battery", client); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("re-registering the name: %v", err)
	}
	if _, err := s.Register(ctx, "alice2", "alice@example.com", "correct horse battery", client); err != nil {
		t.Errorf("reusing the email: %v", err)
	}
	var detail string
	db.QueryRowContext(ctx, `SELECT detail FROM audit_log WHERE action = 'account.deleted'`).Scan(&detail)
	if detail != "@alice" {
		t.Errorf("audit detail = %q", detail)
	}
}
