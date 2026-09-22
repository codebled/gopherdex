package accounts

import (
	"context"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTOTPMatchesRFC6238(t *testing.T) {
	// RFC 6238 appendix B, SHA-1: secret "12345678901234567890", T = 59s → 94287082.
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	if code, _ := totpCode(secret, 59/30); code != "287082" {
		t.Fatalf("code = %s, want 287082 (last six digits of 94287082)", code)
	}
	if code, _ := totpCode(secret, 1111111109/30); code != "081804" {
		t.Fatalf("code = %s, want 081804 (from 07081804)", code)
	}
}

// codeAt returns the current authenticator code for a user, as their app would.
func codeAt(t *testing.T, s *Service, userID int64, at time.Time) string {
	t.Helper()
	var secret string
	s.DB.QueryRow(`SELECT totp_secret FROM users WHERE id = ?`, userID).Scan(&secret)
	code, err := totpCode(secret, at.Unix()/totpPeriod)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestTwoFactorLogin(t *testing.T) {
	s, rec, c := newService(t)
	ctx := context.Background()
	u, _ := s.Register(ctx, "alice", "alice@example.com", "correct horse battery", client)

	setup, err := s.BeginTOTP(ctx, u)
	if err != nil || !strings.HasPrefix(setup.URI, "otpauth://totp/Gopherdex:alice?") || len(setup.Secret) != 32 {
		t.Fatalf("BeginTOTP = %+v, %v", setup, err)
	}
	if _, err := s.ConfirmTOTP(ctx, u, "000000", client); !errors.Is(err, ErrBadSecondFactor) {
		t.Fatalf("wrong confirmation code: %v", err)
	}
	codes, err := s.ConfirmTOTP(ctx, u, codeAt(t, s, u.ID, c.t), client)
	if err != nil || len(codes) != 10 {
		t.Fatalf("ConfirmTOTP = %v, %v", codes, err)
	}

	// The password alone no longer signs in.
	c.t = c.t.Add(time.Minute)
	res, err := s.Login(ctx, "alice", "correct horse battery", client)
	if err != nil || res.Session != "" || res.Challenge == "" || !res.User.TwoFactor {
		t.Fatalf("Login with 2FA = %+v, %v", res, err)
	}
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, "123456", client); !errors.Is(err, ErrBadSecondFactor) {
		t.Fatalf("wrong code: %v", err)
	}
	code := codeAt(t, s, u.ID, c.t)
	session, got, err := s.CompleteLogin(ctx, res.Challenge, code, client)
	if err != nil || session == "" || got.Username != "alice" {
		t.Fatalf("CompleteLogin = %q %+v %v", session, got, err)
	}
	// A challenge works once, and a code can't be replayed.
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, code, client); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("reused challenge: %v", err)
	}
	res, _ = s.Login(ctx, "alice", "correct horse battery", client)
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, code, client); !errors.Is(err, ErrBadSecondFactor) {
		t.Fatalf("replayed code: %v", err)
	}

	// Recovery codes work once each.
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, strings.ToUpper(codes[0]), client); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	res, _ = s.Login(ctx, "alice", "correct horse battery", client)
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, codes[0], client); !errors.Is(err, ErrBadSecondFactor) {
		t.Fatalf("reused recovery code: %v", err)
	}
	if left, _ := s.RecoveryCodesLeft(ctx, u.ID); left != 9 {
		t.Fatalf("recovery codes left = %d", left)
	}

	// Five wrong codes end the challenge.
	res, _ = s.Login(ctx, "alice", "correct horse battery", client)
	for range 5 {
		s.CompleteLogin(ctx, res.Challenge, "000000", client)
	}
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, codeAt(t, s, u.ID, c.t.Add(30*time.Second)), client); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("after 5 wrong codes: %v", err)
	}

	// Turning it off needs a code.
	c.t = c.t.Add(time.Hour)
	if err := s.DisableTOTP(ctx, got, "000000", client); !errors.Is(err, ErrBadSecondFactor) {
		t.Fatalf("disable with wrong code: %v", err)
	}
	if err := s.DisableTOTP(ctx, got, codeAt(t, s, u.ID, c.t), client); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Login(ctx, "alice", "correct horse battery", client)
	if res.Session == "" {
		t.Fatal("password alone should work again after disabling 2FA")
	}

	time.Sleep(50 * time.Millisecond) // security emails are sent in the background
	rec.mu.Lock()
	var subjects []string
	for _, m := range rec.sent {
		subjects = append(subjects, m.Subject)
	}
	rec.mu.Unlock()
	joined := strings.Join(subjects, "|")
	if !strings.Contains(joined, "Two-factor authentication is on") || !strings.Contains(joined, "Two-factor authentication is off") {
		t.Errorf("security emails = %v", subjects)
	}
}

func TestPasswordResetAndChange(t *testing.T) {
	s, rec, _ := newService(t)
	ctx := context.Background()
	u, _ := s.Register(ctx, "alice", "alice@example.com", "correct horse battery", client)
	res1, _ := s.Login(ctx, "alice", "correct horse battery", client)
	res2, _ := s.Login(ctx, "alice", "correct horse battery", client)

	// Unknown emails look the same as known ones.
	if err := s.RequestPasswordReset(ctx, "nobody@example.com", client); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPasswordReset(ctx, "ALICE@example.com", client); err != nil {
		t.Fatal(err)
	}
	body := rec.waitForMail(t, "alice@example.com", "Reset your Gopherdex password") // sent in the background
	i := strings.Index(body, "token=")
	token := strings.Fields(body[i+len("token="):])[0]
	if err := s.CheckResetToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetPassword(ctx, token, "short", client); err == nil {
		t.Fatal("weak password accepted")
	}
	if _, err := s.ResetPassword(ctx, token, "a brand new password", client); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetPassword(ctx, token, "another new password", client); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("reset link reused: %v", err)
	}
	// Every session is signed out.
	for _, r := range []*LoginResult{res1, res2} {
		if _, err := s.SessionUser(ctx, r.Session); !errors.Is(err, ErrInvalidToken) {
			t.Fatal("sessions must end after a reset")
		}
	}
	if _, err := s.Login(ctx, "alice", "correct horse battery", client); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("old password still works")
	}

	// Changing the password keeps only the current session.
	a, _ := s.Login(ctx, "alice", "a brand new password", client)
	b, _ := s.Login(ctx, "alice", "a brand new password", client)
	if err := s.ChangePassword(ctx, u, "wrong", "third password here", a.Session, client); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong current password: %v", err)
	}
	if err := s.ChangePassword(ctx, u, "a brand new password", "third password here", a.Session, client); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, a.Session); err != nil {
		t.Fatal("current session should survive a password change")
	}
	if _, err := s.SessionUser(ctx, b.Session); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("other sessions should end after a password change")
	}

	c, _ := s.Login(ctx, "alice", "third password here", client)
	if n, _ := s.SignOutOthers(ctx, u, c.Session, client); n != 1 {
		t.Fatalf("SignOutOthers = %d, want 1", n)
	}
	events, _ := s.SecurityEvents(ctx, u.ID, 50)
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	for _, want := range []string{"password.reset", "password.changed", "password.change_failed", "sessions.revoked", "login.succeeded"} {
		if !strings.Contains(strings.Join(actions, " "), want) {
			t.Errorf("security log is missing %s: %v", want, actions)
		}
	}
}

func TestModuleScopedToken(t *testing.T) {
	s, rec, _ := newService(t)
	ctx := context.Background()
	u, _ := s.Register(ctx, "alice", "alice@example.com", "correct horse battery", client)
	s.VerifyEmail(ctx, rec.linkToken(t, "alice@example.com"), client)
	u, _ = s.userByID(ctx, u.ID)
	_, tok, err := s.CreateToken(ctx, u, "ci", 0, ScopeModulePrefix+"gopherdex.test/alice/retry", client)
	if err != nil || tok.Scope != "module:gopherdex.test/alice/retry" {
		t.Fatalf("module token = %+v, %v", tok, err)
	}
	if _, _, err := s.CreateToken(ctx, u, "bad", 0, "namespace:bob", client); err == nil {
		t.Fatal("token for someone else's namespace accepted")
	}
}

func TestSecurityHardening(t *testing.T) {
	s, rec, c := newService(t)
	ctx := context.Background()
	u := verifiedUser(t, s, rec, "alice")

	// A pending email change dies with a password reset, and can be canceled.
	if err := s.RequestEmailChange(ctx, u, "attacker@example.com", "correct horse battery", client); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPasswordReset(ctx, "alice@example.com", client); err != nil {
		t.Fatal(err)
	}
	body := rec.waitForMail(t, "alice@example.com", "Reset your Gopherdex password")
	token := strings.Fields(body[strings.Index(body, "token=")+len("token="):])[0]
	if _, err := s.ResetPassword(ctx, token, "a brand new passphrase", client); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PendingEmail(ctx, u); p != "" {
		t.Fatalf("email change to %s survived a password reset", p)
	}
	s.RequestEmailChange(ctx, u, "new@example.com", "a brand new passphrase", client)
	if ok, err := s.CancelEmailChange(ctx, u, client); !ok || err != nil {
		t.Fatalf("cancel: %v, %v", ok, err)
	}
	if p, _ := s.PendingEmail(ctx, u); p != "" {
		t.Fatal("canceled change still pending")
	}

	// ConfirmPassword guards sensitive changes.
	if err := s.ConfirmPassword(ctx, u, "correct horse battery"); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("old password confirmed: %v", err)
	}
	if err := s.ConfirmPassword(ctx, u, "a brand new passphrase"); err != nil {
		t.Errorf("current password: %v", err)
	}

	// Two-factor codes: parallel guesses against one sign-in get five tries.
	if _, err := s.BeginTOTP(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmTOTP(ctx, u, codeAt(t, s, u.ID, c.t), client); err != nil {
		t.Fatal(err)
	}
	failures := func() int {
		var n int
		s.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE user_id = ? AND detail = 'wrong 2FA code'`, u.ID).Scan(&n)
		return n
	}
	res, _ := s.Login(ctx, "alice", "a brand new passphrase", client)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); s.CompleteLogin(ctx, res.Challenge, "000000", client) }()
	}
	wg.Wait()
	if n := failures(); n > maxCodeAttempts {
		t.Fatalf("%d codes checked against one sign-in, want at most %d", n, maxCodeAttempts)
	}
	// Across sign-ins, the account pauses code sign-ins after ten wrong codes,
	// even for the right code, and tells its owner.
	for failures() < maxCodeFailures {
		res, _ := s.Login(ctx, "alice", "a brand new passphrase", client)
		for range maxCodeAttempts {
			if _, _, err := s.CompleteLogin(ctx, res.Challenge, "000000", client); errors.Is(err, ErrTooManyAttempts) || failures() >= maxCodeFailures {
				break
			}
		}
	}
	rec.waitForMail(t, "alice@example.com", "Repeated wrong sign-in codes")
	c.t = c.t.Add(time.Minute)
	res, _ = s.Login(ctx, "alice", "a brand new passphrase", client)
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, codeAt(t, s, u.ID, c.t), client); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("right code during the pause: %v", err)
	}
	c.t = c.t.Add(codeFailureWindow + time.Minute)
	res, _ = s.Login(ctx, "alice", "a brand new passphrase", client)
	if _, _, err := s.CompleteLogin(ctx, res.Challenge, codeAt(t, s, u.ID, c.t), client); err != nil {
		t.Fatalf("right code after the pause: %v", err)
	}
}
