// Package accounts manages users, their namespaces, browser sessions, email
// verification and API tokens.
//
// Every secret handed to a client (session cookie, verification link, API
// token) is 32 random bytes; only its SHA-256 is stored.
package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
)

var (
	// ErrUsernameTaken is returned when an account or organization already
	// has the name.
	ErrUsernameTaken = &FieldError{"username", "That username is taken."}
	// ErrEmailTaken is returned when another account already uses the
	// email address.
	ErrEmailTaken = &FieldError{"email", "An account already uses that email address. Sign in instead."}
	// ErrInvalidCredentials is returned when a sign-in names an unknown
	// account or the wrong password. It doesn't say which, so it can't be
	// used to find out whether an account exists.
	ErrInvalidCredentials = errors.New("incorrect username, email or password")
	// ErrEmailNotVerified is returned when an account whose email address
	// isn't verified yet tries something that needs it, like creating an
	// API token or adding a passkey.
	ErrEmailNotVerified = errors.New("email address is not verified")
	// ErrInvalidToken is returned for a session, link or API token that is
	// unknown, expired, revoked or already used.
	ErrInvalidToken = errors.New("invalid or expired token")
	// ErrNotFound is returned when a looked-up account or record doesn't
	// exist.
	ErrNotFound = errors.New("not found")
	// ErrTooManyTokens is returned by CreateToken when an account already
	// has the maximum number of active API tokens.
	ErrTooManyTokens = errors.New("too many active API tokens")
)

const (
	// SessionTTL is how long a browser session lasts after sign-in.
	SessionTTL      = 14 * 24 * time.Hour
	verificationTTL = 24 * time.Hour
	maxActiveTokens = 20
	// TokenPrefix marks Gopherdex API tokens so secret scanners can find
	// leaked ones.
	TokenPrefix = "gdx_"
)

// User is an account.
type User struct {
	ID            int64
	Username      string
	Email         string
	EmailVerified bool
	TwoFactor     bool // an authenticator app is required to sign in
	CreatedAt     time.Time
}

// Token describes an API token. The secret itself is only returned once, by
// CreateToken.
type Token struct {
	ID         int64
	Name       string
	Prefix     string
	Scope      string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	// PublisherID is set on tokens minted for a trusted publisher; Claims
	// holds the verified CI identity (JSON) that earned the token.
	PublisherID int64
	Claims      string
}

// Client identifies where a request came from, for the audit log.
type Client struct {
	IP        string
	UserAgent string
}

// Service implements account operations.
type Service struct {
	DB      *sql.DB
	Mailer  mail.Mailer
	BaseURL string // public site URL used in emails, e.g. https://gopherdex.dev
	Log     *slog.Logger
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Register creates an account and its namespace, then emails a verification
// link. A failed email does not undo the sign-up; the user can resend it.
func (s *Service) Register(ctx context.Context, username, email, password string, c Client) (*User, error) {
	username, email = NormalizeUsername(username), NormalizeEmail(email)
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if err := validatePassword(password, username, email); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}

	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", username, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT INTO namespaces (name, kind, created_at) VALUES (?, 'user', ?) ON CONFLICT DO NOTHING`, username, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("register %s: create namespace: %w", username, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrUsernameTaken
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO users (username, email, password_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, username, email, hash, now.Unix(), now.Unix())
	if err != nil {
		return nil, fmt.Errorf("register %s: create user: %w", username, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrEmailTaken
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", username, err)
	}
	if err := audit(ctx, tx, id, "account.created", "", c, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("register %s: %w", username, err)
	}

	u := &User{ID: id, Username: username, Email: email, CreatedAt: now}
	if err := s.SendVerification(ctx, u); err != nil {
		s.log().Error("send verification email after sign-up", "user", username, "err", err)
	}
	return u, nil
}

// LoginResult is the outcome of a correct password. Without two-factor
// authentication, Session is set. With it, Challenge is set and the sign-in
// finishes with CompleteLogin.
type LoginResult struct {
	Session   string
	Challenge string
	User      *User
}

// Login checks credentials (username or email plus password).
func (s *Service) Login(ctx context.Context, login, password string, c Client) (*LoginResult, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	var (
		u         User
		hash      string
		verified  sql.NullInt64
		twoFactor sql.NullInt64
		createdAt int64
	)
	err := s.DB.QueryRowContext(ctx, `SELECT id, username, email, password_hash, email_verified_at, `+twoFactorOf("users")+`, created_at
		FROM users WHERE username = ? OR email = ?`, login, login).
		Scan(&u.ID, &u.Username, &u.Email, &hash, &verified, &twoFactor, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Same work as a real check. The result is discarded: the account
		// doesn't exist, so the sign-in fails either way.
		_, _ = checkPassword(password, dummyHash)
		s.auditNoTx(ctx, 0, "login.failed", "unknown account", c)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	ok, err := checkPassword(password, hash)
	if err != nil {
		return nil, fmt.Errorf("login %s: %w", u.Username, err)
	}
	if !ok {
		s.auditNoTx(ctx, u.ID, "login.failed", "wrong password", c)
		return nil, ErrInvalidCredentials
	}
	u.EmailVerified = verified.Valid
	u.TwoFactor = twoFactor.Valid
	u.CreatedAt = time.Unix(createdAt, 0)

	if u.TwoFactor {
		challenge, err := s.createChallenge(ctx, &u)
		if err != nil {
			return nil, err
		}
		return &LoginResult{Challenge: challenge, User: &u}, nil
	}
	secret, err := s.StartSession(ctx, &u, c)
	if err != nil {
		return nil, err
	}
	s.auditNoTx(ctx, u.ID, "login.succeeded", "", c)
	return &LoginResult{Session: secret, User: &u}, nil
}

// StartSession creates a browser session for u, for example right after
// sign-up, and returns the secret to store in a cookie.
func (s *Service) StartSession(ctx context.Context, u *User, c Client) (string, error) {
	secret, digest, err := newSecret()
	if err != nil {
		return "", err
	}
	now := s.now()
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND expires_at <= ?`, u.ID, now.Unix()); err != nil {
		s.log().Warn("delete expired sessions", "user", u.Username, "err", err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, digest, u.ID, now.Unix(), now.Add(SessionTTL).Unix(), now.Unix(), c.IP, truncate(c.UserAgent, 256)); err != nil {
		return "", fmt.Errorf("create session for %s: %w", u.Username, err)
	}
	return secret, nil
}

// SessionUser returns the user a session secret belongs to, or
// ErrInvalidToken if the session is unknown or expired.
func (s *Service) SessionUser(ctx context.Context, secret string) (*User, error) {
	digest, ok := digestSecret(secret)
	if !ok {
		return nil, ErrInvalidToken
	}
	now := s.now()
	var (
		u         User
		verified  sql.NullInt64
		twoFactor sql.NullInt64
		createdAt int64
		lastSeen  int64
	)
	err := s.DB.QueryRowContext(ctx, `SELECT u.id, u.username, u.email, u.email_verified_at, `+twoFactorOf("u")+`, u.created_at, s.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, digest, now.Unix()).
		Scan(&u.ID, &u.Username, &u.Email, &verified, &twoFactor, &createdAt, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("look up session: %w", err)
	}
	u.EmailVerified = verified.Valid
	u.TwoFactor = twoFactor.Valid
	u.CreatedAt = time.Unix(createdAt, 0)
	if now.Unix()-lastSeen > 3600 {
		if _, err := s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, now.Unix(), digest); err != nil {
			s.log().Warn("update session last_seen_at", "err", err)
		}
	}
	return &u, nil
}

// Logout ends a session. Unknown sessions are ignored.
func (s *Service) Logout(ctx context.Context, secret string) error {
	digest, ok := digestSecret(secret)
	if !ok {
		return nil
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, digest); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// SendVerification emails u a link that confirms their address. Earlier
// links for the same user stop working.
func (s *Service) SendVerification(ctx context.Context, u *User) error {
	if u.EmailVerified {
		return nil
	}
	secret, digest, err := newSecret()
	if err != nil {
		return err
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create verification for %s: %w", u.Username, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_verifications WHERE user_id = ?`, u.ID); err != nil {
		return fmt.Errorf("create verification for %s: %w", u.Username, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO email_verifications (token_hash, user_id, email, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, digest, u.ID, u.Email, now.Unix(), now.Add(verificationTTL).Unix()); err != nil {
		return fmt.Errorf("create verification for %s: %w", u.Username, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create verification for %s: %w", u.Username, err)
	}

	link := strings.TrimSuffix(s.BaseURL, "/") + "/verify-email?" + url.Values{"token": {secret}}.Encode()
	return s.Mailer.Send(ctx, mail.Message{
		To:      u.Email,
		Subject: "Verify your email for Gopherdex",
		Body: fmt.Sprintf("Hi %s,\n\nConfirm this is your email address so you can create API tokens and publish Go modules:\n\n%s\n\nThe link expires in 24 hours. If you didn't create a Gopherdex account, ignore this email.\n",
			u.Username, link),
	})
}

// VerifyEmail consumes a verification secret and marks the address verified.
func (s *Service) VerifyEmail(ctx context.Context, secret string, c Client) (*User, error) {
	digest, ok := digestSecret(secret)
	if !ok {
		return nil, ErrInvalidToken
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("verify email: %w", err)
	}
	defer tx.Rollback()

	var userID int64
	var username, current, email string
	err = tx.QueryRowContext(ctx, `SELECT u.id, u.username, u.email, v.email FROM email_verifications v JOIN users u ON u.id = v.user_id
		WHERE v.token_hash = ? AND v.expires_at > ?`, digest, now.Unix()).Scan(&userID, &username, &current, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("verify email: %w", err)
	}
	// A link for a different address confirms an email change: the new
	// address only replaces the old one once someone proves they read it.
	changed := email != current
	if changed {
		res, err := tx.ExecContext(ctx, `UPDATE users SET email = ?, email_verified_at = ?, updated_at = ? WHERE id = ?
			AND NOT EXISTS (SELECT 1 FROM users WHERE email = ? AND id != ?)`, email, now.Unix(), now.Unix(), userID, email, userID)
		if err != nil {
			return nil, fmt.Errorf("change email for %s: %w", username, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrEmailTaken
		}
		if err := audit(ctx, tx, userID, "email.changed", current+" -> "+email, c, now); err != nil {
			return nil, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET email_verified_at = COALESCE(email_verified_at, ?), updated_at = ? WHERE id = ?`, now.Unix(), now.Unix(), userID); err != nil {
			return nil, fmt.Errorf("verify email for %s: %w", username, err)
		}
		if err := audit(ctx, tx, userID, "email.verified", "", c, now); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM email_verifications WHERE user_id = ?`, userID); err != nil {
		return nil, fmt.Errorf("verify email for %s: %w", username, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("verify email for %s: %w", username, err)
	}
	if changed {
		// Tell the old address, in case the account was taken over.
		s.notify(&User{Username: username, Email: current}, "Your Gopherdex email address was changed",
			fmt.Sprintf("The email address for @%s was changed from %s to %s. Emails about the account now go to the new address.", username, current, email))
	}
	return s.userByID(ctx, userID)
}

// Token scopes. A user-wide token acts for everything the user may do; a
// module token can only publish and yank one module.
const (
	scopeUserPrefix   = "namespace:"
	ScopeModulePrefix = "module:"
)

// UserScope is the scope of a token that can act for all of u's modules.
func UserScope(u *User) string { return scopeUserPrefix + u.Username }

// CreateToken issues an API token. An empty scope means all of the user's
// modules; "module:<path>" limits it to one module (the caller checks the
// user maintains it). ttl of zero means the token does not expire. The
// returned secret is shown once and never stored.
func (s *Service) CreateToken(ctx context.Context, u *User, name string, ttl time.Duration, scope string, c Client) (string, *Token, error) {
	if !u.EmailVerified {
		return "", nil, ErrEmailNotVerified
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return "", nil, &FieldError{"name", "Give the token a name of up to 64 characters, like \"laptop\" or \"GitHub Actions\"."}
	}

	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("create token: %w", err)
	}
	defer tx.Rollback()

	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE user_id = ? AND revoked_at IS NULL
		AND (expires_at IS NULL OR expires_at > ?) AND publisher_id IS NULL`, u.ID, now.Unix()).Scan(&active); err != nil {
		return "", nil, fmt.Errorf("create token: %w", err)
	}
	if active >= maxActiveTokens {
		return "", nil, ErrTooManyTokens
	}

	secret, _, err := newSecret()
	if err != nil {
		return "", nil, err
	}
	secret = TokenPrefix + secret
	digest := sha256Sum(secret)
	if scope == "" {
		scope = UserScope(u)
	}
	if scope != UserScope(u) && !strings.HasPrefix(scope, ScopeModulePrefix) {
		return "", nil, fmt.Errorf("invalid token scope %q", scope)
	}
	t := &Token{Name: name, Prefix: secret[:len(TokenPrefix)+6], Scope: scope, CreatedAt: now}
	var expires any
	if ttl > 0 {
		e := now.Add(ttl)
		t.ExpiresAt = &e
		expires = e.Unix()
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (user_id, name, token_hash, prefix, scope, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, u.ID, name, digest, t.Prefix, t.Scope, now.Unix(), expires)
	if err != nil {
		return "", nil, fmt.Errorf("create token: %w", err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return "", nil, fmt.Errorf("create token: %w", err)
	}
	if err := audit(ctx, tx, u.ID, "token.created", fmt.Sprintf("id=%d name=%q", t.ID, name), c, now); err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("create token: %w", err)
	}
	what := "all your modules"
	if m, ok := strings.CutPrefix(scope, ScopeModulePrefix); ok {
		what = m
	}
	s.notify(u, "New Gopherdex API token", fmt.Sprintf("A new API token named %q was created for @%s. It can publish %s.", name, u.Username, what))
	return secret, t, nil
}

// Tokens lists a user's active tokens, newest first.
func (s *Service) Tokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, prefix, scope, created_at, expires_at, last_used_at
		FROM api_tokens WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)
			AND publisher_id IS NULL
		ORDER BY created_at DESC, id DESC`, userID, s.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	tokens := []Token{}
	for rows.Next() {
		var (
			t                 Token
			created           int64
			expires, lastUsed sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Scope, &created, &expires, &lastUsed); err != nil {
			return nil, fmt.Errorf("list tokens: %w", err)
		}
		t.CreatedAt = time.Unix(created, 0)
		t.ExpiresAt = optionalTime(expires)
		t.LastUsedAt = optionalTime(lastUsed)
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// RevokeToken disables one of the user's tokens.
func (s *Service) RevokeToken(ctx context.Context, userID, tokenID int64, c Client) error {
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, now.Unix(), tokenID, userID)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := audit(ctx, tx, userID, "token.revoked", fmt.Sprintf("id=%d", tokenID), c, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}

// AuthenticateToken returns the user and token for an API token secret.
func (s *Service) AuthenticateToken(ctx context.Context, secret string) (*User, *Token, error) {
	if !strings.HasPrefix(secret, TokenPrefix) || len(secret) > 128 {
		return nil, nil, ErrInvalidToken
	}
	now := s.now()
	var (
		u                 User
		t                 Token
		verified          sql.NullInt64
		userCreated       int64
		created           int64
		expires, lastUsed sql.NullInt64
	)
	var twoFactor, publisher sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT u.id, u.username, u.email, u.email_verified_at, `+twoFactorOf("u")+`, u.created_at,
			t.id, t.name, t.prefix, t.scope, t.created_at, t.expires_at, t.last_used_at, t.publisher_id, t.claims
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > ?)`,
		sha256Sum(secret), now.Unix()).
		Scan(&u.ID, &u.Username, &u.Email, &verified, &twoFactor, &userCreated, &t.ID, &t.Name, &t.Prefix, &t.Scope, &created, &expires, &lastUsed, &publisher, &t.Claims)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrInvalidToken
	}
	if err != nil {
		return nil, nil, fmt.Errorf("authenticate token: %w", err)
	}
	u.EmailVerified = verified.Valid
	u.TwoFactor = twoFactor.Valid
	u.CreatedAt = time.Unix(userCreated, 0)
	t.CreatedAt = time.Unix(created, 0)
	t.ExpiresAt = optionalTime(expires)
	t.LastUsedAt = &now
	t.PublisherID = publisher.Int64
	if _, err := s.DB.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now.Unix(), t.ID); err != nil {
		s.log().Warn("update token last_used_at", "token", t.ID, "err", err)
	}
	return &u, &t, nil
}

// MintPublisherToken issues a short-lived token for a trusted publisher,
// acting as the user who set the publisher up. Unlike CreateToken it sends
// no email and doesn't count toward the user's token limit: CI mints one
// per release, and the token expires within minutes.
func (s *Service) MintPublisherToken(ctx context.Context, u *User, name, scope string, ttl time.Duration, publisherID int64, claims string, c Client) (string, *Token, error) {
	if !strings.HasPrefix(scope, ScopeModulePrefix) || ttl <= 0 {
		return "", nil, fmt.Errorf("publisher tokens must be scoped to one module and expire")
	}
	secret, _, err := newSecret()
	if err != nil {
		return "", nil, err
	}
	secret = TokenPrefix + secret
	now := s.now()
	expires := now.Add(ttl)
	t := &Token{Name: truncate(name, 64), Prefix: secret[:len(TokenPrefix)+6], Scope: scope, CreatedAt: now, ExpiresAt: &expires,
		PublisherID: publisherID, Claims: claims}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("mint token: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (user_id, name, token_hash, prefix, scope, created_at, expires_at, publisher_id, claims)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, u.ID, t.Name, sha256Sum(secret), t.Prefix, scope, now.Unix(), expires.Unix(), publisherID, claims)
	if err != nil {
		return "", nil, fmt.Errorf("mint token: %w", err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return "", nil, fmt.Errorf("mint token: %w", err)
	}
	if err := audit(ctx, tx, u.ID, "token.minted", fmt.Sprintf("id=%d publisher=%d %s", t.ID, publisherID, t.Name), c, now); err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("mint token: %w", err)
	}
	return secret, t, nil
}

// UserByID loads an account.
func (s *Service) UserByID(ctx context.Context, id int64) (*User, error) { return s.userByID(ctx, id) }

func (s *Service) userByID(ctx context.Context, id int64) (*User, error) {
	var (
		u         User
		verified  sql.NullInt64
		twoFactor sql.NullInt64
		createdAt int64
	)
	err := s.DB.QueryRowContext(ctx, `SELECT id, username, email, email_verified_at, `+twoFactorOf("users")+`, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.Email, &verified, &twoFactor, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user %d: %w", id, err)
	}
	u.EmailVerified = verified.Valid
	u.TwoFactor = twoFactor.Valid
	u.CreatedAt = time.Unix(createdAt, 0)
	return &u, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func audit(ctx context.Context, db execer, userID int64, action, detail string, c Client, now time.Time) error {
	var uid any
	if userID != 0 {
		uid = userID
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO audit_log (user_id, action, detail, ip, created_at) VALUES (?, ?, ?, ?, ?)`,
		uid, action, detail, c.IP, now.Unix()); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// auditNoTx records an event outside a transaction; failures are logged
// rather than failing the request.
func (s *Service) auditNoTx(ctx context.Context, userID int64, action, detail string, c Client) {
	if err := audit(ctx, s.DB, userID, action, detail, c, s.now()); err != nil {
		s.log().Error("write audit log", "action", action, "err", err)
	}
}

// newSecret returns a random URL-safe secret and its SHA-256 digest.
func newSecret() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("generate secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	return secret, sha256Sum(secret), nil
}

func digestSecret(secret string) ([]byte, bool) {
	if secret == "" || len(secret) > 128 {
		return nil, false
	}
	return sha256Sum(secret), true
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func optionalTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// twoFactorOf is a SQL expression that is non-NULL when the users row
// (under alias) has two-factor authentication: an authenticator app or at
// least one passkey.
func twoFactorOf(alias string) string {
	return "COALESCE(" + alias + ".totp_enabled_at, (SELECT MIN(p.created_at) FROM passkeys p WHERE p.user_id = " + alias + ".id))"
}
