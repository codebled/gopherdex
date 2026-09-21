package accounts

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/database"
	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
)

type recorder struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (r *recorder) Send(_ context.Context, m mail.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, m)
	return nil
}

// linkToken returns the token from the newest verification email to "to".
func (r *recorder) linkToken(t *testing.T, to string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	body := ""
	for _, m := range r.sent {
		if m.To == to {
			body = m.Body
		}
	}
	if body == "" {
		t.Fatalf("no email sent to %s", to)
	}
	i := strings.Index(body, "http")
	link := strings.Fields(body[i:])[0]
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token")
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newService(t *testing.T) (*Service, *recorder, *clock) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rec := &recorder{}
	c := &clock{t: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	return &Service{DB: db, Mailer: rec, BaseURL: "https://gopherdex.test", Now: c.now}, rec, c
}

var client = Client{IP: "192.0.2.1", UserAgent: "test"}

func TestRegisterValidation(t *testing.T) {
	s, _, _ := newService(t)
	ctx := context.Background()
	tests := []struct {
		username, email, password, field string
	}{
		{"a", "a@example.com", "long enough pw", "username"},
		{"-alice", "a@example.com", "long enough pw", "username"},
		{"al--ice", "a@example.com", "long enough pw", "username"},
		{"Alice_1", "a@example.com", "long enough pw", "username"},
		{"api", "a@example.com", "long enough pw", "username"},
		{"v2", "a@example.com", "long enough pw", "username"},
		{strings.Repeat("a", 40), "a@example.com", "long enough pw", "username"},
		{"alice", "not-an-email", "long enough pw", "email"},
		{"alice", "Alice <a@example.com>", "long enough pw", "email"},
		{"alice", "a@example.com", "short", "password"},
		{"alice", "a@example.com", "ALICE", "password"},
	}
	for _, tt := range tests {
		_, err := s.Register(ctx, tt.username, tt.email, tt.password, client)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != tt.field {
			t.Errorf("Register(%q, %q, %q) = %v, want error on %s", tt.username, tt.email, tt.password, err, tt.field)
		}
	}
}

func TestRegisterLoginVerifyTokens(t *testing.T) {
	s, rec, c := newService(t)
	ctx := context.Background()

	u, err := s.Register(ctx, " Alice ", "Alice@Example.com", "correct horse battery", client)
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" || u.Email != "alice@example.com" || u.EmailVerified {
		t.Fatalf("user = %+v", u)
	}
	if _, err := s.Register(ctx, "alice", "other@example.com", "correct horse battery", client); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate username: %v", err)
	}
	if _, err := s.Register(ctx, "bob", "ALICE@example.com", "correct horse battery", client); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email: %v", err)
	}
	// A failed registration must not leave its namespace behind.
	if _, err := s.Register(ctx, "bob", "bob@example.com", "correct horse battery", client); err != nil {
		t.Fatalf("bob after rollback: %v", err)
	}

	// Login by username or email; wrong password and unknown user fail the same way.
	if _, err := s.Login(ctx, "alice", "wrong password!", client); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := s.Login(ctx, "nobody", "correct horse battery", client); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	res, err := s.Login(ctx, "ALICE@example.com", "correct horse battery", client)
	if err != nil {
		t.Fatal(err)
	}
	session := res.Session
	if got, err := s.SessionUser(ctx, session); err != nil || got.Username != "alice" {
		t.Fatalf("SessionUser = %+v, %v", got, err)
	}

	// Tokens need a verified email.
	if _, _, err := s.CreateToken(ctx, u, "laptop", 0, "", client); !errors.Is(err, ErrEmailNotVerified) {
		t.Fatalf("CreateToken before verification: %v", err)
	}
	link := rec.linkToken(t, "alice@example.com")
	if _, err := s.VerifyEmail(ctx, "bogus", client); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("bogus verification: %v", err)
	}
	u, err = s.VerifyEmail(ctx, link, client)
	if err != nil || !u.EmailVerified {
		t.Fatalf("VerifyEmail = %+v, %v", u, err)
	}
	if _, err := s.VerifyEmail(ctx, link, client); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("verification link reused: %v", err)
	}

	secret, tok, err := s.CreateToken(ctx, u, "laptop", 30*24*time.Hour, "", client)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) || tok.Scope != "namespace:alice" || !strings.HasPrefix(secret, tok.Prefix) {
		t.Fatalf("token %q = %+v", secret, tok)
	}
	gotUser, gotTok, err := s.AuthenticateToken(ctx, secret)
	if err != nil || gotUser.Username != "alice" || gotTok.ID != tok.ID {
		t.Fatalf("AuthenticateToken = %+v %+v %v", gotUser, gotTok, err)
	}
	list, err := s.Tokens(ctx, u.ID)
	if err != nil || len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatalf("Tokens = %+v, %v", list, err)
	}

	if err := s.RevokeToken(ctx, u.ID+1, tok.ID, client); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking another user's token: %v", err)
	}
	if err := s.RevokeToken(ctx, u.ID, tok.ID, client); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateToken(ctx, secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token still works: %v", err)
	}

	// Expiry.
	secret, _, err = s.CreateToken(ctx, u, "ci", time.Hour, "", client)
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(2 * time.Hour)
	if _, _, err := s.AuthenticateToken(ctx, secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token still works: %v", err)
	}
	c.t = c.t.Add(SessionTTL)
	if _, err := s.SessionUser(ctx, session); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired session still works: %v", err)
	}

	if err := s.Logout(ctx, session); err != nil {
		t.Fatal(err)
	}

	var failed int
	s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action = 'login.failed'`).Scan(&failed)
	if failed != 2 {
		t.Errorf("login.failed audit rows = %d, want 2", failed)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("s3cret password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash = %q", h)
	}
	if ok, err := checkPassword("s3cret password", h); !ok || err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if ok, _ := checkPassword("s3cret passworD", h); ok {
		t.Fatal("wrong password accepted")
	}
	if _, err := checkPassword("x", "$2a$10$bcrypt"); err == nil {
		t.Fatal("foreign hash format accepted")
	}
}
