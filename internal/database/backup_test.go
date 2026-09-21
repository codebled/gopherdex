package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`INSERT INTO namespaces (name, kind, created_at) VALUES ('alice', 'user', 1)`)

	out := filepath.Join(dir, "copy.db")
	if err := Backup(ctx, db, out); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, db, out); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	copyDB, err := Open(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	copyDB.QueryRow(`SELECT COUNT(*) FROM namespaces WHERE name = 'alice'`).Scan(&n)
	copyDB.Close()
	if n != 1 {
		t.Fatal("backup is missing data")
	}

	backups := filepath.Join(dir, "backups")
	start := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		if _, err := BackupTo(ctx, db, backups, 3, start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(backups)
	if len(entries) != 3 || entries[0].Name() != "gopherdex-20260918T020000Z.db" {
		t.Fatalf("kept backups = %v", entries)
	}
}
