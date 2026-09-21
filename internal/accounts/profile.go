package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
)

// ErrLastOrgOwner blocks deleting an account that is the only owner of an
// organization, which would leave nobody able to manage it.
type ErrLastOrgOwner struct{ Orgs []string }

func (e *ErrLastOrgOwner) Error() string {
	return "you're the only owner of " + strings.Join(e.Orgs, ", ")
}

// checkCurrentPassword verifies u's current password, recording failures.
func (s *Service) checkCurrentPassword(ctx context.Context, u *User, password, action string, c Client) error {
	var hash string
	if err := s.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash); err != nil {
		return fmt.Errorf("check password: %w", err)
	}
	ok, err := checkPassword(password, hash)
	if err != nil {
		return err
	}
	if !ok {
		s.auditNoTx(ctx, u.ID, action, "wrong current password", c)
		return ErrWrongPassword
	}
	return nil
}

// RequestEmailChange sends a confirmation link to newEmail. The account's
// address changes only when that link is opened (see VerifyEmail); until
// then everything keeps going to the current address.
func (s *Service) RequestEmailChange(ctx context.Context, u *User, newEmail, password string, c Client) error {
	newEmail = NormalizeEmail(newEmail)
	if err := validateEmail(newEmail); err != nil {
		return err
	}
	if newEmail == u.Email {
		return &FieldError{"email", "That's already your email address."}
	}
	if err := s.checkCurrentPassword(ctx, u, password, "email.change_failed", c); err != nil {
		return err
	}
	var taken bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = ?)`, newEmail).Scan(&taken); err != nil {
		return fmt.Errorf("change email: %w", err)
	}
	if taken {
		return &FieldError{"email", "Another account uses that email address."}
	}

	secret, digest, err := newSecret()
	if err != nil {
		return err
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("change email: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_verifications WHERE user_id = ?`, u.ID); err != nil {
		return fmt.Errorf("change email: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO email_verifications (token_hash, user_id, email, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, digest, u.ID, newEmail, now.Unix(), now.Add(verificationTTL).Unix()); err != nil {
		return fmt.Errorf("change email: %w", err)
	}
	if err := audit(ctx, tx, u.ID, "email.change_requested", newEmail, c, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("change email: %w", err)
	}

	link := strings.TrimSuffix(s.BaseURL, "/") + "/verify-email?" + url.Values{"token": {secret}}.Encode()
	if err := s.Mailer.Send(ctx, mail.Message{
		To:      newEmail,
		Subject: "Confirm your new email for Gopherdex",
		Body: fmt.Sprintf("Hi %s,\n\nOpen this link to make %s the email address of your Gopherdex account:\n\n%s\n\nThe link expires in 24 hours. If you didn't ask for this, ignore this email; nothing changes.\n",
			u.Username, newEmail, link),
	}); err != nil {
		return fmt.Errorf("send confirmation to %s: %w", newEmail, err)
	}
	s.notify(u, "Email change requested for your Gopherdex account",
		fmt.Sprintf("Someone signed in as @%s asked to change the account's email address to %s. It changes only if the link sent there is opened.", u.Username, newEmail))
	return nil
}

// PendingEmail returns the address an unconfirmed email change is waiting
// on, or "".
func (s *Service) PendingEmail(ctx context.Context, u *User) (string, error) {
	var email string
	err := s.DB.QueryRowContext(ctx, `SELECT email FROM email_verifications WHERE user_id = ? AND email != ? AND expires_at > ?`,
		u.ID, u.Email, s.now().Unix()).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return email, err
}

// Preferences are the optional emails a user receives.
type Preferences struct {
	Publish bool // a version of one of my modules was published
	Access  bool // I was given access to a module or organization
}

// Preferences returns u's email preferences.
func (s *Service) Preferences(ctx context.Context, userID int64) (Preferences, error) {
	var p Preferences
	err := s.DB.QueryRowContext(ctx, `SELECT notify_publish, notify_access FROM users WHERE id = ?`, userID).Scan(&p.Publish, &p.Access)
	if err != nil {
		return p, fmt.Errorf("load preferences: %w", err)
	}
	return p, nil
}

// SetPreferences saves u's email preferences.
func (s *Service) SetPreferences(ctx context.Context, u *User, p Preferences, c Client) error {
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET notify_publish = ?, notify_access = ?, updated_at = ? WHERE id = ?`,
		p.Publish, p.Access, s.now().Unix(), u.ID); err != nil {
		return fmt.Errorf("save preferences: %w", err)
	}
	s.auditNoTx(ctx, u.ID, "preferences.changed", fmt.Sprintf("publish=%v access=%v", p.Publish, p.Access), c)
	return nil
}

// Email kinds for EmailsFor.
const (
	EmailPublish = "publish"
	EmailAccess  = "access"
)

// EmailsFor returns the addresses of the named users who want emails of
// kind (EmailPublish or EmailAccess).
func (s *Service) EmailsFor(ctx context.Context, usernames []string, kind string) (map[string]string, error) {
	column := map[string]string{EmailPublish: "notify_publish", EmailAccess: "notify_access"}[kind]
	if column == "" {
		return nil, fmt.Errorf("unknown email kind %q", kind)
	}
	out := map[string]string{}
	for _, name := range usernames {
		var email string
		err := s.DB.QueryRowContext(ctx, `SELECT email FROM users WHERE username = ? AND `+column+` = 1`, name).Scan(&email)
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

// DeleteAccount permanently deletes u after checking their password and,
// when two-factor is on, a code. Their sessions, tokens and trusted
// publishers go with it. Published modules stay: versions are immutable
// and other programs depend on them. The username stays reserved so
// nobody can take over its module paths.
func (s *Service) DeleteAccount(ctx context.Context, u *User, password, code string, c Client) error {
	if err := s.checkCurrentPassword(ctx, u, password, "account.delete_failed", c); err != nil {
		return err
	}
	if u.TwoFactor {
		if err := s.verifySecondFactor(ctx, u.ID, code); err != nil {
			s.auditNoTx(ctx, u.ID, "account.delete_failed", "wrong 2FA code", c)
			return err
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT o.name FROM organizations o JOIN org_members m ON m.org_id = o.id
		WHERE m.user_id = ? AND m.role = 'owner'
		  AND (SELECT COUNT(*) FROM org_members WHERE org_id = o.id AND role = 'owner') = 1
		ORDER BY o.name`, u.ID)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	var orgs []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		orgs = append(orgs, "@"+name)
	}
	rows.Close()
	if len(orgs) > 0 {
		return &ErrLastOrgOwner{Orgs: orgs}
	}

	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	defer tx.Rollback()
	// The audit row outlives the user (its user_id becomes NULL), so it
	// names them.
	if err := audit(ctx, tx, u.ID, "account.deleted", "@"+u.Username, c, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	s.log().Info("account deleted", "user", u.Username)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := s.Mailer.Send(ctx, mail.Message{To: u.Email, Subject: "Your Gopherdex account was deleted",
			Body: fmt.Sprintf("Hi %s,\n\nYour Gopherdex account @%s was deleted. Modules you published stay available, because programs depend on them, and the name @%s can't be registered again.\n\nIf you didn't do this, contact the registry's administrators right away.\n", u.Username, u.Username, u.Username)})
		if err != nil {
			s.log().Error("send account deletion email", "user", u.Username, "err", err)
		}
	}()
	return nil
}
