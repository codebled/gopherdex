package database

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
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
	var mmap int64
	if err := db.QueryRowContext(ctx, `PRAGMA mmap_size`).Scan(&mmap); err != nil || mmap < 1<<30 {
		t.Fatalf("mmap_size = %d, %v; want memory-mapped reads", mmap, err)
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

func TestCheckpointRestartsWAL(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "wal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insert := func(from, n int) {
		for i := range n {
			if _, err := db.ExecContext(ctx, `INSERT INTO namespaces (name, kind, created_at) VALUES (?, 'user', 1)`, fmt.Sprintf("user%d", from+i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A small log is only copied, never restarted.
	insert(0, 50)
	if frames, restarted, err := CheckpointOnce(ctx, db); err != nil || restarted || frames == 0 {
		t.Fatalf("small log: %d frames, restarted %v, %v", frames, restarted, err)
	}
	// Past the threshold it restarts, even with a reader mid-query.
	var blob = strings.Repeat("x", 3000)
	tx, _ := db.BeginTx(ctx, nil)
	for i := range restartAbove / 2 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO namespaces (name, kind, created_at) VALUES (?, 'user', 1)`, fmt.Sprintf("big%d-%s", i, blob)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM namespaces`)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next() // reading the latest snapshot doesn't block a restart
	frames, restarted, err := CheckpointOnce(ctx, db)
	rows.Close()
	if err != nil || !restarted || frames <= restartAbove {
		t.Fatalf("large log: %d frames, restarted %v, %v", frames, restarted, err)
	}
	// The connection used for it is back on the normal busy timeout.
	for range 5 {
		var ms int
		db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&ms)
		if ms != 5000 {
			t.Fatalf("busy_timeout = %d after a checkpoint", ms)
		}
	}
}
