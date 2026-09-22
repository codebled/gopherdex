package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Keeping the write-ahead log small under load.
//
// SQLite's automatic checkpoints are passive: they copy the log into the
// database but only restart the log when no reader is on an older snapshot.
// On a busy registry, with uploads, sign-ins and download counts written
// every few milliseconds, some read always is, so the log never restarts:
// load testing grew it from nothing to 956 MB in 30 minutes. Reads get
// slower as it grows, because only the main file is memory-mapped.
//
// A RESTART checkpoint fixes that. It holds new writers back, waits for the
// reads already running (new reads see the latest data, so they don't block
// it), copies everything and lets the next writer start the log over. It
// waits at most restartWait, so a write is held up for at most that long;
// if a slow read is still running, it tries again at the next tick.
// (A TRUNCATE checkpoint with the normal 5-second busy timeout stalled
// writes for 5 seconds every attempt, and still didn't finish.)

const (
	restartAbove = 16384 // log pages (64 MB at 4 KB) before forcing a restart
	restartWait  = 250 * time.Millisecond
)

// Checkpoint keeps the write-ahead log small until ctx ends.
func Checkpoint(ctx context.Context, db *sql.DB, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		frames, restarted, err := CheckpointOnce(ctx, db)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("checkpoint the write-ahead log", "err", err)
		case restarted:
			misses = 0
		case frames > restartAbove:
			if misses++; misses%12 == 0 { // once a minute at 5-second ticks
				log.Warn("write-ahead log keeps growing: slow reads block checkpoints", "pages", frames)
			}
		}
	}
}

// CheckpointOnce copies the log into the database without blocking anyone,
// and when the log is over restartAbove pages, restarts it, waiting at most
// restartWait for reads in progress. It returns the log's size in pages
// and whether it was restarted.
func CheckpointOnce(ctx context.Context, db *sql.DB) (frames int, restarted bool, err error) {
	var busy, logFrames, done int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &done); err != nil {
		return 0, false, err
	}
	if logFrames <= restartAbove {
		return logFrames, false, nil
	}
	// A dedicated connection, so the short busy timeout applies only here.
	conn, err := db.Conn(ctx)
	if err != nil {
		return logFrames, false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, restartWait.Milliseconds())); err != nil {
		return logFrames, false, err
	}
	defer func() {
		// Back to Open's setting before the pool reuses the connection.
		if _, resetErr := conn.ExecContext(context.Background(), `PRAGMA busy_timeout = 5000`); resetErr != nil && err == nil {
			err = fmt.Errorf("reset busy timeout: %w", resetErr)
		}
	}()
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(RESTART)`).Scan(&busy, &logFrames, &done); err != nil {
		return logFrames, false, err
	}
	return logFrames, busy == 0, nil
}
