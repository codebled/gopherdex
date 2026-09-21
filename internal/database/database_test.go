package database

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestOpenMigratesOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "gopherdex.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var fk int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v; want 1", fk, err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	db.Close()

	// Reopening must not re-run migrations.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, _ := fs.Glob(migrations, "migrations/*.sql")
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(files) {
		t.Fatalf("schema_migrations rows = %d, %v; want %d (one per migration file)", n, err, len(files))
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users (username, email, password_hash, created_at, updated_at) VALUES ('ghost', 'g@example.com', 'x', 0, 0)`); err == nil {
		t.Fatal("inserting a user without a namespace should violate the foreign key")
	}
}
