// Package database opens the registry's SQLite database and applies its
// schema migrations.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens (creating if needed) the SQLite database at path and migrates
// it to the latest schema.
//
// The connection enables foreign keys, WAL journaling for concurrent readers,
// a busy timeout so writers queue instead of failing, immediate
// transactions so a read-then-write transaction cannot deadlock, and
// memory-mapped reads.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	q := url.Values{}
	// mmap_size lets reads come straight from the OS page cache, shared by
	// every connection, instead of one pread system call per page: load
	// testing a 4.6 GB database spent three quarters of its CPU in pread.
	// cache_size keeps a connection's hottest pages (8 MB each).
	for _, p := range []string{"foreign_keys(1)", "journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)",
		"mmap_size(2147483648)", "cache_size(-8000)", "journal_size_limit(67108864)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// Queries run on the CPU (the driver is pure Go), so connections beyond
	// the core count add no throughput. More matter for the write-ahead log:
	// it has only a few reader slots, and dozens of overlapping readers
	// keep some slot pinned to an old position, so checkpoints never catch
	// up and the log grows without end (load testing, 32 clients).
	db.SetMaxOpenConns(maxConns())
	db.SetMaxIdleConns(maxConns())
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate applies every embedded migration that has not run yet. Files are
// named NNN_description.sql and run in order, each in its own transaction.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if err := apply(ctx, db, version, name); err != nil {
			return err
		}
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	base := filepath.Base(name)
	num, _, ok := strings.Cut(base, "_")
	v, err := strconv.Atoi(num)
	if !ok || err != nil || v <= 0 {
		return 0, fmt.Errorf("migration %s must be named NNN_description.sql", base)
	}
	return v, nil
}

func apply(ctx context.Context, db *sql.DB, version int, name string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}
	defer tx.Rollback()

	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied); err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}
	if applied > 0 {
		return nil
	}
	body, err := fs.ReadFile(migrations, name)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, time.Now().Unix()); err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}
	return nil
}

// maxConns is the connection pool size: one per CPU core, at least 4.
func maxConns() int { return max(4, runtime.GOMAXPROCS(0)) }
