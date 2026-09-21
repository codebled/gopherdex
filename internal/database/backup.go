package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const backupPrefix = "gopherdex-"

// Backup writes a consistent copy of the database to path with SQLite's
// VACUUM INTO, which is safe while the server keeps serving requests. It
// refuses to overwrite an existing file.
//
// Published module zips live in the blob directory, not the database. They
// are write-once files, so copy that directory with any file backup tool.
func Backup(ctx context.Context, db *sql.DB, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup %s: file already exists", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("backup %s: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("backup %s: %w", path, err)
	}
	return nil
}

// BackupTo writes a timestamped backup into dir, then deletes all but the
// newest keep backups there. It returns the new file's path.
func BackupTo(ctx context.Context, db *sql.DB, dir string, keep int, now time.Time) (string, error) {
	path := filepath.Join(dir, backupPrefix+now.UTC().Format("20060102T150405Z")+".db")
	if err := Backup(ctx, db, path); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return path, err
	}
	var backups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), backupPrefix) && strings.HasSuffix(e.Name(), ".db") {
			backups = append(backups, e.Name())
		}
	}
	slices.Sort(backups) // timestamps sort chronologically
	for len(backups) > max(keep, 1) {
		if err := os.Remove(filepath.Join(dir, backups[0])); err != nil {
			return path, fmt.Errorf("remove old backup: %w", err)
		}
		backups = backups[1:]
	}
	return path, nil
}
