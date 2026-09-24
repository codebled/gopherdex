package accounts

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/codebled/gopherdex/internal/webauthntest"
)

func TestPasskeys(t *testing.T) {
	s, rec, _ := newService(t)
	ctx := context.Background()
	alice := verifiedUser(t, s, rec, "alice")
	key := webauthntest.NewSoftKey(t, "gopherdex.test", "https://gopherdex.test")

	// Register.
	options, token, err := s.BeginPasskeyRegistration(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if options.Response.RelyingParty.ID != "gopherdex.test" || options.Response.AuthenticatorSelection.ResidentKey != protocol.ResidentKeyRequirementRequired {
		t.Errorf("options = %+v", options.Response)
	}
	pk, codes, err := s.FinishPasskeyRegistration(ctx, alice, token, "MacBook Touch ID", bytes.NewReader(key.Create(t, options)), client)
	if err != nil {
		t.Fatal(err)
	}
	if pk.Name != "MacBook Touch ID" || !pk.Synced || len(codes) != recoveryCodes {
		t.Errorf("passkey = %+v, %d recovery codes", pk, len(codes))
	}
	rec.waitForMail(t, "alice@example.com", "A passkey was added")
	if _, _, err := s.FinishPasskeyRegistration(ctx, alice, token, "again", bytes.NewReader(key.Create(t, options)), client); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("reusing a registration: %v", err)
	}
	if u, _ := s.userByID(ctx, alice.ID); !u.TwoFactor {
		t.Error("an account with a passkey doesn't count as having two-factor")
	}

	// Sign in without a password.
	login, token, err := s.BeginPasskeyLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session, u, err := s.FinishPasskeyLogin(ctx, token, bytes.NewReader(key.Get(t, login)), client)
	if err != nil || u.ID != alice.ID {
		t.Fatalf("passkey sign-in: %v, %v", u, err)
	}
	if got, err := s.SessionUser(ctx, session); err != nil || got.ID != alice.ID {
		t.Errorf("session: %v, %v", got, err)
	}
	if list, _ := s.Passkeys(ctx, alice.ID); len(list) != 1 || list[0].LastUsedAt == nil {
		t.Errorf("passkeys = %+v", list)
	}

	// A passkey from another site, or a replayed answer, is refused.
	login, token, _ = s.BeginPasskeyLogin(ctx)
	phished := *key
	phished.Origin = "https://gopherdex-login.example"
	if _, _, err := s.FinishPasskeyLogin(ctx, token, bytes.NewReader(phished.Get(t, login)), client); !errors.Is(err, ErrPasskey) {
		t.Errorf("wrong origin: %v", err)
	}
	old := key.Get(t, login)
	_, token, _ = s.BeginPasskeyLogin(ctx)
	if _, _, err := s.FinishPasskeyLogin(ctx, token, bytes.NewReader(old), client); !errors.Is(err, ErrPasskey) {
		t.Errorf("answer to an old challenge: %v", err)
	}
	// A copied passkey (its counter goes backwards) is refused.
	login, token, _ = s.BeginPasskeyLogin(ctx)
	clone := *key
	clone.Counter = 0
	if _, _, err := s.FinishPasskeyLogin(ctx, token, bytes.NewReader(clone.Get(t, login)), client); !errors.Is(err, ErrPasskey) {
		t.Errorf("cloned passkey: %v", err)
	}

	// With a password, the passkey answers the second step.
	res, err := s.Login(ctx, "alice", "correct horse battery", client)
	if err != nil || res.Challenge == "" {
		t.Fatalf("password sign-in should ask for a second factor: %+v, %v", res, err)
	}
	second, token, err := s.BeginPasskeySecondFactor(ctx, res.Challenge)
	if err != nil || len(second.Response.AllowedCredentials) != 1 {
		t.Fatalf("second factor options: %+v, %v", second, err)
	}
	if _, u, err := s.FinishPasskeySecondFactor(ctx, res.Challenge, token, bytes.NewReader(key.Get(t, second)), client); err != nil || u.ID != alice.ID {
		t.Fatalf("second factor: %v", err)
	}
	// ...and a recovery code works too, for when the device is lost.
	res, _ = s.Login(ctx, "alice", "correct horse battery", client)
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, codes[0], client); err != nil {
		t.Errorf("recovery code: %v", err)
	}

	// Removing the last passkey turns two-factor off and drops the codes.
	list, _ := s.Passkeys(ctx, alice.ID)
	if err := s.DeletePasskey(ctx, alice, list[0].ID, "wrong", client); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("remove with a wrong password: %v", err)
	}
	if err := s.DeletePasskey(ctx, alice, list[0].ID, "correct horse battery", client); err != nil {
		t.Fatal(err)
	}
	rec.waitForMail(t, "alice@example.com", "A passkey was removed")
	if u, _ := s.userByID(ctx, alice.ID); u.TwoFactor {
		t.Error("two-factor still on without passkeys or an app")
	}
	if n, _ := s.RecoveryCodesLeft(ctx, alice.ID); n != 0 {
		t.Errorf("%d recovery codes left", n)
	}
	if res, _ := s.Login(ctx, "alice", "correct horse battery", client); res.Session == "" {
		t.Error("password alone should sign in again")
	}
}
