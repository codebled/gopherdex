package registry

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Downloads counts module zips served by this registry's proxy.
//
// Counts are buffered in memory and written in batches, so a popular module
// doesn't turn every download into a database write. Downloads that the
// public Go module mirror serves from its own cache never reach this
// registry, so these numbers are a lower bound.
type Downloads struct {
	Registry *Registry
	Every    time.Duration // flush interval; default 15s

	mu      sync.Mutex
	pending map[downloadKey]int
	stop    chan struct{}
	done    chan struct{}
}

type downloadKey struct {
	module, version string
	day             int64
}

// Count records one download. It never blocks on the database.
func (d *Downloads) Count(modPath, version string) {
	day := d.Registry.now().UTC().Unix() / 86400
	d.mu.Lock()
	if d.pending == nil {
		d.pending = map[downloadKey]int{}
	}
	d.pending[downloadKey{modPath, version, day}]++
	d.mu.Unlock()
}

// Start flushes counts in the background until Stop.
func (d *Downloads) Start() {
	every := d.Every
	if every <= 0 {
		every = 15 * time.Second
	}
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(d.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				d.flushAndLog()
			case <-d.stop:
				d.flushAndLog()
				return
			}
		}
	}()
}

// Stop writes any buffered counts and stops the background flusher.
func (d *Downloads) Stop() {
	if d.stop == nil {
		d.flushAndLog()
		return
	}
	close(d.stop)
	<-d.done
}

func (d *Downloads) flushAndLog() {
	if err := d.Flush(context.Background()); err != nil {
		d.Registry.log().Error("flush download counts", "err", err)
	}
}

// Flush writes buffered counts now. Counts that fail to write are kept for
// the next attempt.
func (d *Downloads) Flush(ctx context.Context) error {
	d.mu.Lock()
	batch := d.pending
	d.pending = nil
	d.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	err := func() error {
		tx, err := d.Registry.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for k, n := range batch {
			if _, err := tx.ExecContext(ctx, `INSERT INTO downloads (module_id, version, day, count)
				SELECT id, ?, ?, ? FROM modules WHERE path = ?
				ON CONFLICT (module_id, version, day) DO UPDATE SET count = count + excluded.count`,
				k.version, k.day, n, k.module); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	if err != nil {
		d.mu.Lock()
		if d.pending == nil {
			d.pending = map[downloadKey]int{}
		}
		for k, n := range batch {
			d.pending[k] += n
		}
		d.mu.Unlock()
		return fmt.Errorf("write %d download counts: %w", len(batch), err)
	}
	return nil
}

// DownloadStats summarizes a module's downloads.
type DownloadStats struct {
	LastDay, LastWeek, LastMonth, Total int
	Daily                               []DayCount     // the last 30 days, oldest first, zeros included
	ByVersion                           map[string]int // all time
}

// DayCount is the downloads on one UTC day.
type DayCount struct {
	Day   time.Time
	Count int
}

// ModuleDownloads returns download statistics for a module.
func (r *Registry) ModuleDownloads(ctx context.Context, modPath string) (DownloadStats, error) {
	today := r.now().UTC().Unix() / 86400
	stats := DownloadStats{ByVersion: map[string]int{}}
	perDay := map[int64]int{}
	rows, err := r.DB.QueryContext(ctx, `SELECT d.version, d.day, d.count FROM downloads d JOIN modules m ON m.id = d.module_id WHERE m.path = ?`, modPath)
	if err != nil {
		return stats, fmt.Errorf("download stats for %s: %w", modPath, err)
	}
	defer rows.Close()
	for rows.Next() {
		var version string
		var day int64
		var n int
		if err := rows.Scan(&version, &day, &n); err != nil {
			return stats, err
		}
		stats.Total += n
		stats.ByVersion[version] += n
		perDay[day] += n
		switch age := today - day; {
		case age < 1:
			stats.LastDay += n
			fallthrough
		case age < 7:
			stats.LastWeek += n
			fallthrough
		case age < 30:
			stats.LastMonth += n
		}
	}
	for day := today - 29; day <= today; day++ {
		stats.Daily = append(stats.Daily, DayCount{Day: time.Unix(day*86400, 0).UTC(), Count: perDay[day]})
	}
	return stats, rows.Err()
}

// PopularModules lists modules by downloads over the last 30 days.
func (r *Registry) PopularModules(ctx context.Context, limit int) ([]SearchHit, error) {
	hits, _, err := r.Search(ctx, SearchQuery{Sort: SortDownloads, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := hits[:0]
	for _, h := range hits {
		if h.Downloads30 > 0 {
			out = append(out, h)
		}
	}
	return out, nil
}
