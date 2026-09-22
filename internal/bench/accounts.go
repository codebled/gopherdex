package bench

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/database"
)

// AccountsConfig describes credentials to prepare for write load.
type AccountsConfig struct {
	DB         string // a seeded benchmark database
	Out        string // accounts.tsv
	Publishers int    // existing publishers to give API tokens, so they can release new versions
	NewUsers   int    // fresh accounts with known passwords, for sign-ins and brand-new modules
}

// Account is one line of accounts.tsv.
type Account struct {
	Kind     string // "publisher" (owns seeded modules) or "user" (fresh, owns nothing yet)
	Username string
	Password string // only for "user"
	Token    string
}

// PrepareAccounts creates the credentials the load generator's write
// scenarios use, straight in a benchmark database (the server can keep
// running: SQLite's WAL lets both write). These are throwaway secrets for
// a local test database; the file is written with owner-only permissions.
func PrepareAccounts(ctx context.Context, cfg AccountsConfig) (int, error) {
	db, err := database.Open(ctx, cfg.DB)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	acc := &accounts.Service{DB: db, Mailer: nopMailer{}, BaseURL: "http://localhost"}
	c := accounts.Client{IP: "192.0.2.1"}
	var out []Account

	// The busiest real publishers first: they have modules to release.
	rows, err := db.QueryContext(ctx, `SELECT u.id FROM users u JOIN modules m ON m.created_by = u.id
		WHERE u.email LIKE '%@bench.invalid' GROUP BY u.id ORDER BY COUNT(*) DESC, u.id LIMIT ?`, cfg.Publishers)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		u, err := acc.UserByID(ctx, id)
		if err != nil {
			return 0, err
		}
		secret, _, err := acc.CreateToken(ctx, u, "load test", 7*24*time.Hour, "", c)
		if err != nil {
			return 0, fmt.Errorf("token for %s: %w", u.Username, err)
		}
		out = append(out, Account{Kind: "publisher", Username: u.Username, Token: secret})
	}

	stamp := time.Now().Format("0102-1504")
	for i := range cfg.NewUsers {
		name := fmt.Sprintf("load-%s-%d", stamp, i)
		pw := randomSecret()
		u, err := acc.Register(ctx, name, name+"@bench.invalid", pw, c)
		if err != nil {
			return 0, fmt.Errorf("register %s: %w", name, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE users SET email_verified_at = ? WHERE id = ?`, time.Now().Unix(), u.ID); err != nil {
			return 0, err
		}
		u.EmailVerified = true
		secret, _, err := acc.CreateToken(ctx, u, "load test", 7*24*time.Hour, "", c)
		if err != nil {
			return 0, err
		}
		out = append(out, Account{Kind: "user", Username: name, Password: pw, Token: secret})
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Out), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(cfg.Out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(f)
	for _, a := range out {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Kind, a.Username, a.Password, a.Token)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return 0, err
	}
	return len(out), f.Close()
}

func randomSecret() string {
	b := make([]byte, 18)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// readAccounts loads accounts.tsv.
func readAccounts(path string) ([]Account, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Account
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 4 {
			out = append(out, Account{f[0], f[1], f[2], f[3]})
		}
	}
	return out, nil
}
