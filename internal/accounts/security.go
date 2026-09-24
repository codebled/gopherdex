package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/codebled/gopherdex/internal/mail"
)

var (
	// ErrBadSecondFactor is returned when an authenticator or recovery
	// code doesn't match.
	ErrBadSecondFactor = errors.New("that code isn't right")
	// ErrTooManyAttempts is returned when a pending sign-in, or the account
	// within the last hour, has had too many wrong codes.
	ErrTooManyAttempts = errors.New("too many wrong codes; sign in again")
	// ErrWrongPassword is returned when a sensitive change is confirmed
	// with the wrong current password.
	ErrWrongPassword = &FieldError{"current_password", "Your current password isn't right."}
)

const (
	resetTTL        = time.Hour
	challengeTTL    = 5 * time.Minute
	maxCodeAttempts = 5 // per sign-in

	// maxCodeFailures wrong two-factor codes per codeFailureWindow pause
	// code sign-ins for the account, whatever address they come from.
	maxCodeFailures   = 10
	codeFailureWindow = time.Hour
	recoveryCodes     = 10
	issuer            = "Gopherdex"
)

// notify emails u about a change to their account in the background, so a
// slow mail server never delays the request. Failures are logged.
func (s *Service) notify(u *User, subject, body string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		msg := mail.Message{To: u.Email, Subject: subject, Body: fmt.Sprintf("Hi %s,\n\n%s\n\nIf this wasn't you, reset your password at %s/forgot-password and review %s/account/security right away.\n",
			u.Username, body, strings.TrimSuffix(s.BaseURL, "/"), strings.TrimSuffix(s.BaseURL, "/"))}
		if err := s.Mailer.Send(ctx, msg); err != nil {
			s.log().Error("send security email", "user", u.Username, "subject", subject, "err", err)
		}
	}()
}

// ---- Password reset ----

// RequestPasswordReset emails a reset link if an account uses email. It
// behaves the same whether or not the account exists, so the form can't be
// used to discover who has an account.
func (s *Service) RequestPasswordReset(ctx context.Context, email string, c Client) error {
	email = NormalizeEmail(email)
	u, err := s.userByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		s.auditNoTx(ctx, 0, "password.reset_requested", "unknown email", c)
		return nil
	}
	if err != nil {
		return err
	}
	secret, digest, err := newSecret()
	if err != nil {
		return err
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM password_resets WHERE user_id = ?`, u.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO password_resets (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		digest, u.ID, now.Unix(), now.Add(resetTTL).Unix()); err != nil {
		return fmt.Errorf("create password reset: %w", err)
	}
	if err := audit(ctx, tx, u.ID, "password.reset_requested", "", c, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	link := strings.TrimSuffix(s.BaseURL, "/") + "/reset-password?" + url.Values{"token": {secret}}.Encode()
	msg := mail.Message{
		To:      u.Email,
		Subject: "Reset your Gopherdex password",
		Body: fmt.Sprintf("Hi %s,\n\nSomeone asked to reset the password for @%s. Choose a new one here:\n\n%s\n\nThe link works once and expires in an hour. If you didn't ask for this, ignore this email; your password hasn't changed.\n",
			u.Username, u.Username, link),
	}
	// Sent in the background, so the response takes as long for an address
	// with an account as for one without: timing can't reveal accounts.
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.Mailer.Send(ctx, msg); err != nil {
			s.log().Error("send password reset email", "user", u.Username, "err", err)
		}
	}()
	return nil
}

// CheckResetToken reports whether a reset link is still valid.
func (s *Service) CheckResetToken(ctx context.Context, secret string) error {
	digest, ok := digestSecret(secret)
	if !ok {
		return ErrInvalidToken
	}
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM password_resets WHERE token_hash = ? AND expires_at > ?`, digest, s.now().Unix()).Scan(&n); err != nil {
		return fmt.Errorf("check reset token: %w", err)
	}
	if n == 0 {
		return ErrInvalidToken
	}
	return nil
}

// ResetPassword sets a new password from a reset link and signs the
// account out everywhere.
func (s *Service) ResetPassword(ctx context.Context, secret, password string, c Client) (*User, error) {
	digest, ok := digestSecret(secret)
	if !ok {
		return nil, ErrInvalidToken
	}
	now := s.now()
	var userID int64
	err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM password_resets WHERE token_hash = ? AND expires_at > ?`, digest, now.Unix()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("reset password: %w", err)
	}
	u, err := s.userByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err := validatePassword(password, u.Username, u.Email); err != nil {
		return nil, err
	}
	if err := s.setPassword(ctx, u, password, "", "password.reset", c); err != nil {
		return nil, err
	}
	s.notify(u, "Your Gopherdex password was reset", "The password for @"+u.Username+" was just reset using an email link. Every session was signed out and any pending email change was canceled. API tokens and passkeys still work: review them on your account and security pages.")
	return u, nil
}

// ChangePassword replaces the password of a signed-in user after checking
// the current one. Every other session is signed out.
func (s *Service) ChangePassword(ctx context.Context, u *User, current, next, keepSession string, c Client) error {
	var hash string
	if err := s.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash); err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	ok, err := checkPassword(current, hash)
	if err != nil {
		return err
	}
	if !ok {
		s.auditNoTx(ctx, u.ID, "password.change_failed", "wrong current password", c)
		return ErrWrongPassword
	}
	if err := validatePassword(next, u.Username, u.Email); err != nil {
		return err
	}
	if err := s.setPassword(ctx, u, next, keepSession, "password.changed", c); err != nil {
		return err
	}
	s.notify(u, "Your Gopherdex password was changed", "The password for @"+u.Username+" was just changed. Other signed-in sessions were signed out.")
	return nil
}

func (s *Service) setPassword(ctx context.Context, u *User, password, keepSession, action string, c Client) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	now := s.now()
	keep, _ := digestSecret(keepSession)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []struct {
		q    string
		args []any
	}{
		{`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, []any{hash, now.Unix(), u.ID}},
		{`DELETE FROM sessions WHERE user_id = ? AND token_hash IS NOT ?`, []any{u.ID, keep}},
		{`DELETE FROM password_resets WHERE user_id = ?`, []any{u.ID}},
		{`DELETE FROM login_challenges WHERE user_id = ?`, []any{u.ID}},
		// A pending email change may be an attacker's: after a password
		// reset it would otherwise still let them take the account back.
		{`DELETE FROM email_verifications WHERE user_id = ? AND email != (SELECT email FROM users WHERE id = ?)`, []any{u.ID, u.ID}},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return fmt.Errorf("set password: %w", err)
		}
	}
	if err := audit(ctx, tx, u.ID, action, "", c, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- Sessions and security log ----

// SessionCount returns how many sessions the user has open.
func (s *Service) SessionCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?`, userID, s.now().Unix()).Scan(&n)
	return n, err
}

// SignOutOthers ends every session of u except the one given.
func (s *Service) SignOutOthers(ctx context.Context, u *User, keepSession string, c Client) (int, error) {
	keep, _ := digestSecret(keepSession)
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash IS NOT ?`, u.ID, keep)
	if err != nil {
		return 0, fmt.Errorf("sign out other sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	// Whoever held another session may have started an email change.
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM email_verifications WHERE user_id = ? AND email != (SELECT email FROM users WHERE id = ?)`, u.ID, u.ID); err != nil {
		return int(n), fmt.Errorf("cancel email change: %w", err)
	}
	s.auditNoTx(ctx, u.ID, "sessions.revoked", fmt.Sprintf("%d sessions", n), c)
	return int(n), nil
}

// Event is one entry of a user's security log.
type Event struct {
	Action string
	Detail string
	IP     string
	At     time.Time
}

// SecurityEvents returns a user's most recent account and publishing events.
func (s *Service) SecurityEvents(ctx context.Context, userID int64, limit int) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT action, detail, ip, created_at FROM audit_log WHERE user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("security log: %w", err)
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.Action, &e.Detail, &e.IP, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}

// ---- Two-factor authentication ----

// TOTPSetup is shown while a user adds an authenticator app.
type TOTPSetup struct {
	Secret string // base32, for typing in by hand
	URI    string // otpauth:// URI, for the QR code
}

// BeginTOTP creates (or shows again) the secret for adding an authenticator
// app. Two-factor login isn't on until ConfirmTOTP succeeds.
func (s *Service) BeginTOTP(ctx context.Context, u *User) (*TOTPSetup, error) {
	// Passkeys count as two-factor too; what matters here is the app.
	if on, err := s.AppEnabled(ctx, u.ID); err != nil {
		return nil, err
	} else if on {
		return nil, errors.New("an authenticator app is already set up")
	}
	var secret string
	if err := s.DB.QueryRowContext(ctx, `SELECT totp_secret FROM users WHERE id = ?`, u.ID).Scan(&secret); err != nil {
		return nil, err
	}
	if secret == "" {
		var err error
		if secret, err = newTOTPSecret(); err != nil {
			return nil, err
		}
		if _, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_secret = ? WHERE id = ? AND totp_enabled_at IS NULL`, secret, u.ID); err != nil {
			return nil, fmt.Errorf("start 2FA setup: %w", err)
		}
	}
	return &TOTPSetup{Secret: secret, URI: totpURI(issuer, u.Username, secret)}, nil
}

// ConfirmTOTP turns two-factor login on once the user proves their app
// works, and returns one-time recovery codes to keep somewhere safe.
func (s *Service) ConfirmTOTP(ctx context.Context, u *User, code string, c Client) ([]string, error) {
	var secret string
	var enabled sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `SELECT totp_secret, totp_enabled_at FROM users WHERE id = ?`, u.ID).Scan(&secret, &enabled); err != nil {
		return nil, err
	}
	if secret == "" || enabled.Valid {
		return nil, errors.New("start two-factor setup first")
	}
	step := matchTOTP(secret, code, s.now(), 0)
	if step == 0 {
		return nil, ErrBadSecondFactor
	}
	codes := make([]string, recoveryCodes)
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_enabled_at = ?, totp_last_step = ? WHERE id = ?`, now.Unix(), step, u.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, u.ID); err != nil {
		return nil, err
	}
	for i := range codes {
		if codes[i], err = newRecoveryCode(); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)`, u.ID, sha256Sum(codes[i])); err != nil {
			return nil, err
		}
	}
	if err := audit(ctx, tx, u.ID, "2fa.enabled", "", c, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.notify(u, "Two-factor authentication is on", "Two-factor authentication was turned on for @"+u.Username+". Signing in now needs a code from your authenticator app.")
	return codes, nil
}

// DisableTOTP turns two-factor login off after checking a current code or
// recovery code.
func (s *Service) DisableTOTP(ctx context.Context, u *User, code string, c Client) error {
	if err := s.verifySecondFactor(ctx, u.ID, code); err != nil {
		s.auditNoTx(ctx, u.ID, "2fa.disable_failed", "", c)
		return err
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_secret = '', totp_enabled_at = NULL, totp_last_step = 0 WHERE id = ?`, u.ID); err != nil {
		return err
	}
	// Recovery codes stay while passkeys remain; they recover those too.
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ? AND NOT EXISTS (SELECT 1 FROM passkeys WHERE user_id = ?)`, u.ID, u.ID); err != nil {
		return err
	}
	if err := audit(ctx, tx, u.ID, "2fa.disabled", "", c, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify(u, "Two-factor authentication is off", "Two-factor authentication was turned off for @"+u.Username+". Signing in now needs only your password.")
	return nil
}

// RecoveryCodesLeft counts unused recovery codes.
func (s *Service) RecoveryCodesLeft(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

// verifySecondFactor accepts a current authenticator code (never the same
// one twice) or an unused recovery code (which is then used up).
func (s *Service) verifySecondFactor(ctx context.Context, userID int64, code string) error {
	var secret string
	var lastStep int64
	var enabled sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `SELECT totp_secret, totp_last_step, totp_enabled_at FROM users WHERE id = ?`, userID).
		Scan(&secret, &lastStep, &enabled); err != nil {
		return err
	}
	if !enabled.Valid {
		// Passkey-only accounts still have recovery codes.
		if has, err := s.HasPasskeys(ctx, userID); err != nil {
			return err
		} else if !has {
			return errors.New("two-factor authentication isn't on")
		}
	} else if step := matchTOTP(secret, code, s.now(), lastStep); step != 0 {
		res, err := s.DB.ExecContext(ctx, `UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, userID, step)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		return ErrBadSecondFactor // raced with another use of the same code
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		s.now().Unix(), userID, sha256Sum(normalizeRecoveryCode(code)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	return ErrBadSecondFactor
}

func (s *Service) createChallenge(ctx context.Context, u *User) (string, error) {
	secret, digest, err := newSecret()
	if err != nil {
		return "", err
	}
	now := s.now()
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM login_challenges WHERE expires_at <= ?`, now.Unix()); err != nil {
		s.log().Warn("delete expired login challenges", "err", err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO login_challenges (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		digest, u.ID, now.Unix(), now.Add(challengeTTL).Unix()); err != nil {
		return "", fmt.Errorf("create login challenge: %w", err)
	}
	return secret, nil
}

// CompleteLogin finishes a two-factor sign-in: it checks the code for the
// challenge Login returned and starts a session.
func (s *Service) CompleteLogin(ctx context.Context, challenge, code string, c Client) (string, *User, error) {
	digest, ok := digestSecret(challenge)
	if !ok {
		return "", nil, ErrInvalidToken
	}
	// Each attempt is counted before the code is checked, in one statement,
	// so parallel guesses against one challenge can't all see a low count.
	var userID int64
	err := s.DB.QueryRowContext(ctx, `UPDATE login_challenges SET attempts = attempts + 1
		WHERE token_hash = ? AND expires_at > ? AND attempts < ? RETURNING user_id`,
		digest, s.now().Unix(), maxCodeAttempts).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := s.DB.ExecContext(ctx, `DELETE FROM login_challenges WHERE token_hash = ?`, digest)
		if err != nil {
			return "", nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return "", nil, ErrTooManyAttempts
		}
		return "", nil, ErrInvalidToken
	}
	if err != nil {
		return "", nil, err
	}
	// Across challenges, too: someone who knows the password can start a
	// new sign-in for five more guesses, from as many addresses as they like.
	var failures int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE user_id = ? AND action = 'login.failed'
		AND detail = 'wrong 2FA code' AND created_at > ?`, userID, s.now().Add(-codeFailureWindow).Unix()).Scan(&failures); err != nil {
		return "", nil, err
	}
	if failures >= maxCodeFailures {
		return "", nil, ErrTooManyAttempts
	}
	if err := s.verifySecondFactor(ctx, userID, code); err != nil {
		s.auditNoTx(ctx, userID, "login.failed", "wrong 2FA code", c)
		if failures+1 == maxCodeFailures {
			if u, uerr := s.userByID(ctx, userID); uerr == nil {
				s.notify(u, "Repeated wrong sign-in codes on your Gopherdex account",
					fmt.Sprintf("Someone signed in to @%s with the right password but entered a wrong two-factor code %d times in the last hour. "+
						"Your password is probably known to someone else. Change it now. Sign-ins with codes are paused for an hour; passkeys still work.",
						u.Username, maxCodeFailures))
			}
		}
		return "", nil, err
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM login_challenges WHERE token_hash = ?`, digest); err != nil {
		s.log().Error("delete login challenge", "user_id", userID, "err", err)
	}
	u, err := s.userByID(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	session, err := s.StartSession(ctx, u, c)
	if err != nil {
		return "", nil, err
	}
	s.auditNoTx(ctx, u.ID, "login.succeeded", "with 2FA", c)
	return session, u, nil
}

func (s *Service) userByEmail(ctx context.Context, email string) (*User, error) {
	var id int64
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE email = ?`, email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.userByID(ctx, id)
}

// UserEmails returns the email addresses of the given users, for
// notifications.
func (s *Service) UserEmails(ctx context.Context, usernames []string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range usernames {
		var email string
		err := s.DB.QueryRowContext(ctx, `SELECT email FROM users WHERE username = ?`, name).Scan(&email)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[name] = email
	}
	return out, nil
}

// Notify emails u about a change to their account or modules, with advice
// on what to do if they didn't make it.
func (s *Service) Notify(u *User, subject, body string) { s.notify(u, subject, body) }

// ConfirmPassword checks u's current password before a sensitive change,
// such as creating an API token, turning on an authenticator app or adding
// a passkey, so a stolen session alone can't plant lasting access.
func (s *Service) ConfirmPassword(ctx context.Context, u *User, password string) error {
	var hash string
	if err := s.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash); err != nil {
		return err
	}
	ok, err := checkPassword(password, hash)
	if err != nil {
		return err
	}
	if !ok {
		return ErrWrongPassword
	}
	return nil
}

// CancelEmailChange drops a pending change of u's email address.
func (s *Service) CancelEmailChange(ctx context.Context, u *User, c Client) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM email_verifications WHERE user_id = ? AND email != (SELECT email FROM users WHERE id = ?)`, u.ID, u.ID)
	if err != nil {
		return false, fmt.Errorf("cancel email change: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.auditNoTx(ctx, u.ID, "email.change_cancelled", "", c) //nolint:misspell // audit action name already stored in audit_log rows
	}
	return n > 0, nil
}
