package accounts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Passkeys (WebAuthn) let people sign in with Touch ID, Face ID, Windows
// Hello or a security key. Signing in with one asks the device to verify
// the person (fingerprint, face or PIN), so a passkey alone is two
// factors: something they have and something they are or know. A passkey
// also answers the second step after a password, like an authenticator
// code. Accounts with a passkey count as having two-factor authentication.

var (
	// ErrPasskey is returned when a passkey answer can't be parsed or
	// doesn't verify: a wrong origin, a stale challenge, an unknown
	// credential or a counter that went backwards.
	ErrPasskey = errors.New("that passkey didn't verify; try again")
	// ErrTooManyPasskeys is returned when an account that already has the
	// maximum number of passkeys adds another.
	ErrTooManyPasskeys = &FieldError{"name", "An account can have at most 10 passkeys. Remove one first."}
)

const (
	maxPasskeys  = 10
	ceremonyTTL  = 5 * time.Minute
	rpName       = "Gopherdex"
	userHandleSz = 32
)

// Passkey is a registered passkey, as shown on the security page.
type Passkey struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	Synced     bool // backed up by a password manager or platform (iCloud Keychain, Google…)
}

// relyingParty configures WebAuthn for the site: its domain is the
// relying party ID, and only pages from its origin may use the passkeys.
func (s *Service) relyingParty() (*webauthn.WebAuthn, error) {
	u, err := url.Parse(s.BaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("passkeys need a base URL, got %q", s.BaseURL)
	}
	return webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: rpName,
		RPOrigins:     []string{u.Scheme + "://" + u.Host},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL},
		},
	})
}

// waUser adapts an account to go-webauthn.
type waUser struct {
	u      *User
	handle []byte
	creds  []webauthn.Credential
}

// WebAuthnID returns the account's random user handle, which authenticators
// store instead of the account ID so passkeys don't reveal it.
func (w *waUser) WebAuthnID() []byte { return w.handle }

// WebAuthnName returns the username, which authenticators show when
// someone picks a passkey.
func (w *waUser) WebAuthnName() string { return w.u.Username }

// WebAuthnDisplayName returns the username as Gopherdex shows it, with a
// leading @.
func (w *waUser) WebAuthnDisplayName() string { return "@" + w.u.Username }

// WebAuthnCredentials returns the account's registered passkeys.
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

// loadWAUser loads an account's passkeys, creating its user handle if
// create is set and it has none yet.
func (s *Service) loadWAUser(ctx context.Context, u *User, create bool) (*waUser, error) {
	w := &waUser{u: u}
	if err := s.DB.QueryRowContext(ctx, `SELECT webauthn_handle FROM users WHERE id = ?`, u.ID).Scan(&w.handle); err != nil {
		return nil, fmt.Errorf("load passkeys: %w", err)
	}
	if len(w.handle) == 0 && create {
		w.handle = make([]byte, userHandleSz)
		if _, err := rand.Read(w.handle); err != nil {
			return nil, err
		}
		if _, err := s.DB.ExecContext(ctx, `UPDATE users SET webauthn_handle = ? WHERE id = ? AND webauthn_handle IS NULL`, w.handle, u.ID); err != nil {
			return nil, fmt.Errorf("create user handle: %w", err)
		}
		if err := s.DB.QueryRowContext(ctx, `SELECT webauthn_handle FROM users WHERE id = ?`, u.ID).Scan(&w.handle); err != nil {
			return nil, err
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT credential FROM passkeys WHERE user_id = ? ORDER BY id`, u.ID)
	if err != nil {
		return nil, fmt.Errorf("load passkeys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, fmt.Errorf("decode passkey: %w", err)
		}
		w.creds = append(w.creds, c)
	}
	return w, rows.Err()
}

// startCeremony stores a WebAuthn session and returns the secret that
// redeems it (kept in a short-lived cookie by the server).
func (s *Service) startCeremony(ctx context.Context, userID int64, kind string, session *webauthn.SessionData) (string, error) {
	data, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	secret, digest, err := newSecret()
	if err != nil {
		return "", err
	}
	now := s.now()
	var uid any
	if userID != 0 {
		uid = userID
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM webauthn_ceremonies WHERE expires_at < ?`, now.Unix()); err != nil {
		return "", err
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO webauthn_ceremonies (token_hash, user_id, kind, session, expires_at) VALUES (?, ?, ?, ?, ?)`,
		digest, uid, kind, string(data), now.Add(ceremonyTTL).Unix()); err != nil {
		return "", fmt.Errorf("start passkey ceremony: %w", err)
	}
	return secret, nil
}

// takeCeremony redeems a ceremony once.
func (s *Service) takeCeremony(ctx context.Context, secret string, userID int64, kind string) (*webauthn.SessionData, error) {
	digest, ok := digestSecret(secret)
	if !ok {
		return nil, ErrInvalidToken
	}
	var raw string
	var owner sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `DELETE FROM webauthn_ceremonies WHERE token_hash = ? AND kind = ? AND expires_at > ?
		RETURNING session, user_id`, digest, kind, s.now().Unix()).Scan(&raw, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if owner.Int64 != userID {
		return nil, ErrInvalidToken
	}
	var session webauthn.SessionData
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// BeginPasskeyRegistration returns the options for navigator.credentials.
// create() and the secret that finishes the registration.
func (s *Service) BeginPasskeyRegistration(ctx context.Context, u *User) (*protocol.CredentialCreation, string, error) {
	if !u.EmailVerified {
		return nil, "", ErrEmailNotVerified
	}
	rp, err := s.relyingParty()
	if err != nil {
		return nil, "", err
	}
	w, err := s.loadWAUser(ctx, u, true)
	if err != nil {
		return nil, "", err
	}
	if len(w.creds) >= maxPasskeys {
		return nil, "", ErrTooManyPasskeys
	}
	exclude := make([]protocol.CredentialDescriptor, len(w.creds))
	for i, c := range w.creds {
		exclude[i] = c.Descriptor()
	}
	options, session, err := rp.BeginRegistration(w,
		webauthn.WithExclusions(exclude),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired, // a real passkey: usable without typing a username
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		return nil, "", fmt.Errorf("begin passkey registration: %w", err)
	}
	token, err := s.startCeremony(ctx, u.ID, "register", session)
	if err != nil {
		return nil, "", err
	}
	return options, token, nil
}

// FinishPasskeyRegistration checks the browser's response and saves the
// passkey. If this is the account's first second factor, it also returns
// new recovery codes, to show once.
func (s *Service) FinishPasskeyRegistration(ctx context.Context, u *User, token, name string, response io.Reader, c Client) (*Passkey, []string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if len([]rune(name)) > 60 {
		return nil, nil, &FieldError{"name", "Keep the passkey's name under 60 characters."}
	}
	session, err := s.takeCeremony(ctx, token, u.ID, "register")
	if err != nil {
		return nil, nil, err
	}
	rp, err := s.relyingParty()
	if err != nil {
		return nil, nil, err
	}
	w, err := s.loadWAUser(ctx, u, false)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(response)
	if err != nil {
		return nil, nil, ErrPasskey
	}
	cred, err := rp.CreateCredential(w, *session, parsed)
	if err != nil {
		s.log().Info("passkey registration refused", "user", u.Username, "err", err)
		return nil, nil, ErrPasskey
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return nil, nil, err
	}

	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM passkeys WHERE user_id = ?`, u.ID).Scan(&count); err != nil {
		return nil, nil, err
	}
	if count >= maxPasskeys {
		return nil, nil, ErrTooManyPasskeys
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO passkeys (user_id, credential_id, name, credential, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (credential_id) DO NOTHING`, u.ID, cred.ID, name, string(data), now.Unix())
	if err != nil {
		return nil, nil, fmt.Errorf("save passkey: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil, &FieldError{"name", "That passkey is already registered."}
	}
	id, _ := res.LastInsertId()
	// Someone who relies on passkeys alone needs a way back in if they lose
	// their devices; they get recovery codes the first time.
	var codes []string
	var unused int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, u.ID).Scan(&unused); err != nil {
		return nil, nil, err
	}
	if unused == 0 {
		if codes, err = replaceRecoveryCodes(ctx, tx, u.ID); err != nil {
			return nil, nil, err
		}
	}
	if err := audit(ctx, tx, u.ID, "passkey.added", name, c, now); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	s.notify(u, "A passkey was added to your Gopherdex account",
		fmt.Sprintf("A passkey named %q was added to @%s. It can now sign in to the account.", name, u.Username))
	return &Passkey{ID: id, Name: name, CreatedAt: now, Synced: cred.Flags.BackupState}, codes, nil
}

// replaceRecoveryCodes issues a fresh set of one-time recovery codes.
func replaceRecoveryCodes(ctx context.Context, tx *sql.Tx, userID int64) ([]string, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	codes := make([]string, recoveryCodes)
	for i := range codes {
		var err error
		if codes[i], err = newRecoveryCode(); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)`, userID, sha256Sum(codes[i])); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

// BeginPasskeyLogin returns options for signing in with any passkey the
// browser offers, without a username.
func (s *Service) BeginPasskeyLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	rp, err := s.relyingParty()
	if err != nil {
		return nil, "", err
	}
	options, session, err := rp.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", fmt.Errorf("begin passkey sign-in: %w", err)
	}
	token, err := s.startCeremony(ctx, 0, "login", session)
	if err != nil {
		return nil, "", err
	}
	return options, token, nil
}

// FinishPasskeyLogin verifies a passkey sign-in and starts a session.
func (s *Service) FinishPasskeyLogin(ctx context.Context, token string, response io.Reader, c Client) (string, *User, error) {
	session, err := s.takeCeremony(ctx, token, 0, "login")
	if err != nil {
		return "", nil, err
	}
	rp, err := s.relyingParty()
	if err != nil {
		return "", nil, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(response)
	if err != nil {
		return "", nil, ErrPasskey
	}
	var found *waUser
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		var id int64
		err := s.DB.QueryRowContext(ctx, `SELECT u.id FROM users u JOIN passkeys p ON p.user_id = u.id
			WHERE u.webauthn_handle = ? AND p.credential_id = ?`, userHandle, rawID).Scan(&id)
		if err != nil {
			return nil, ErrPasskey
		}
		u, err := s.userByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if found, err = s.loadWAUser(ctx, u, false); err != nil {
			return nil, err
		}
		return found, nil
	}
	_, cred, err := rp.ValidatePasskeyLogin(handler, *session, parsed)
	if err != nil || found == nil {
		s.log().Info("passkey sign-in refused", "err", err)
		s.auditNoTx(ctx, 0, "login.failed", "passkey didn't verify", c)
		return "", nil, ErrPasskey
	}
	if err := s.usePasskey(ctx, found.u, cred, c); err != nil {
		return "", nil, err
	}
	secret, err := s.StartSession(ctx, found.u, c)
	if err != nil {
		return "", nil, err
	}
	s.auditNoTx(ctx, found.u.ID, "login.succeeded", "with passkey", c)
	return secret, found.u, nil
}

// BeginPasskeySecondFactor returns options for answering the second step
// of a password sign-in with one of the account's passkeys.
func (s *Service) BeginPasskeySecondFactor(ctx context.Context, challenge string) (*protocol.CredentialAssertion, string, error) {
	userID, err := s.challengeUser(ctx, challenge)
	if err != nil {
		return nil, "", err
	}
	u, err := s.userByID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	w, err := s.loadWAUser(ctx, u, false)
	if err != nil {
		return nil, "", err
	}
	if len(w.creds) == 0 {
		return nil, "", ErrPasskey
	}
	rp, err := s.relyingParty()
	if err != nil {
		return nil, "", err
	}
	options, session, err := rp.BeginLogin(w, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return nil, "", fmt.Errorf("begin passkey check: %w", err)
	}
	token, err := s.startCeremony(ctx, u.ID, "second-factor", session)
	if err != nil {
		return nil, "", err
	}
	return options, token, nil
}

// FinishPasskeySecondFactor completes a password sign-in with a passkey.
func (s *Service) FinishPasskeySecondFactor(ctx context.Context, challenge, token string, response io.Reader, c Client) (string, *User, error) {
	userID, err := s.challengeUser(ctx, challenge)
	if err != nil {
		return "", nil, err
	}
	session, err := s.takeCeremony(ctx, token, userID, "second-factor")
	if err != nil {
		return "", nil, err
	}
	u, err := s.userByID(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	w, err := s.loadWAUser(ctx, u, false)
	if err != nil {
		return "", nil, err
	}
	rp, err := s.relyingParty()
	if err != nil {
		return "", nil, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(response)
	if err != nil {
		return "", nil, ErrPasskey
	}
	cred, err := rp.ValidateLogin(w, *session, parsed)
	if err != nil {
		s.auditNoTx(ctx, u.ID, "login.failed", "passkey didn't verify", c)
		return "", nil, ErrPasskey
	}
	if err := s.usePasskey(ctx, u, cred, c); err != nil {
		return "", nil, err
	}
	if digest, ok := digestSecret(challenge); ok {
		if _, err := s.DB.ExecContext(ctx, `DELETE FROM login_challenges WHERE token_hash = ?`, digest); err != nil {
			s.log().Error("delete login challenge", "user", u.Username, "err", err)
		}
	}
	secret, err := s.StartSession(ctx, u, c)
	if err != nil {
		return "", nil, err
	}
	s.auditNoTx(ctx, u.ID, "login.succeeded", "with password and passkey", c)
	return secret, u, nil
}

// challengeUser returns the account a pending password sign-in belongs to.
func (s *Service) challengeUser(ctx context.Context, challenge string) (int64, error) {
	digest, ok := digestSecret(challenge)
	if !ok {
		return 0, ErrInvalidToken
	}
	var userID int64
	err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM login_challenges WHERE token_hash = ? AND expires_at > ? AND attempts < ?`,
		digest, s.now().Unix(), maxCodeAttempts).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrInvalidToken
	}
	return userID, err
}

// usePasskey records a successful use: the new signature counter and the
// time. A counter that went backwards means the passkey may have been
// copied, so the sign-in is refused.
func (s *Service) usePasskey(ctx context.Context, u *User, cred *webauthn.Credential, c Client) error {
	if cred.Authenticator.CloneWarning {
		s.auditNoTx(ctx, u.ID, "login.failed", "passkey signature counter went backwards", c)
		return ErrPasskey
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE passkeys SET credential = ?, last_used_at = ? WHERE user_id = ? AND credential_id = ?`,
		string(data), s.now().Unix(), u.ID, cred.ID)
	return err
}

// Passkeys lists an account's passkeys, oldest first.
func (s *Service) Passkeys(ctx context.Context, userID int64) ([]Passkey, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, credential, created_at, last_used_at FROM passkeys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		var raw string
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Name, &raw, &created, &used); err != nil {
			return nil, err
		}
		var c webauthn.Credential
		if json.Unmarshal([]byte(raw), &c) == nil {
			p.Synced = c.Flags.BackupState
		}
		p.CreatedAt = time.Unix(created, 0).UTC()
		p.LastUsedAt = optionalTime(used)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePasskey removes one of u's passkeys after checking their password.
func (s *Service) DeletePasskey(ctx context.Context, u *User, id int64, password string, c Client) error {
	if err := s.checkCurrentPassword(ctx, u, password, "passkey.remove_failed", c); err != nil {
		return err
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	if err := tx.QueryRowContext(ctx, `DELETE FROM passkeys WHERE id = ? AND user_id = ? RETURNING name`, id, u.ID).Scan(&name); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	// Without passkeys or an authenticator app, recovery codes have
	// nothing left to recover.
	var remaining int
	var totp sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM passkeys WHERE user_id = ?), totp_enabled_at FROM users WHERE id = ?`, u.ID, u.ID).
		Scan(&remaining, &totp); err != nil {
		return err
	}
	if remaining == 0 && !totp.Valid {
		if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, u.ID); err != nil {
			return err
		}
	}
	if err := audit(ctx, tx, u.ID, "passkey.removed", name, c, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify(u, "A passkey was removed from your Gopherdex account",
		fmt.Sprintf("The passkey named %q was removed from @%s and can no longer sign in.", name, u.Username))
	return nil
}

// HasPasskeys reports whether an account has any passkeys.
func (s *Service) HasPasskeys(ctx context.Context, userID int64) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM passkeys WHERE user_id = ?`, userID).Scan(&n)
	return n > 0, err
}

// ChallengeMethods reports which second factors a pending password sign-in
// can use: an authenticator app, passkeys, or both. Recovery codes work
// whenever either does.
func (s *Service) ChallengeMethods(ctx context.Context, challenge string) (app, passkeys bool, err error) {
	userID, err := s.challengeUser(ctx, challenge)
	if err != nil {
		return false, false, err
	}
	var totp sql.NullInt64
	var n int
	err = s.DB.QueryRowContext(ctx, `SELECT totp_enabled_at, (SELECT COUNT(*) FROM passkeys WHERE user_id = ?) FROM users WHERE id = ?`, userID, userID).
		Scan(&totp, &n)
	return totp.Valid, n > 0, err
}

// AppEnabled reports whether an account uses an authenticator app.
func (s *Service) AppEnabled(ctx context.Context, userID int64) (bool, error) {
	var totp sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT totp_enabled_at FROM users WHERE id = ?`, userID).Scan(&totp)
	return totp.Valid, err
}
