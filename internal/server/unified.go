package server

import (
	"context"
	"sync"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/discovery"
	"github.com/parthiban-sivakumar/gopherdex/internal/ranking"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

const (
	unifiedLimit   = 40 // results in the combined list
	hostedPool     = 60 // hosted candidates considered for it
	publicTimeout  = 1500 * time.Millisecond
	publicCacheTTL = 10 * time.Minute
	publicCacheCap = 512
)

// publicCache keeps recent pkg.go.dev results, so paging back and forth or
// a popular query doesn't wait on the network each time.
type publicCache struct {
	mu    sync.Mutex
	items map[string]publicEntry
}

type publicEntry struct {
	results []discovery.Result
	at      time.Time
}

func (c *publicCache) get(q string) ([]discovery.Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[q]
	if !ok || time.Since(e.at) > publicCacheTTL {
		return nil, false
	}
	return e.results, true
}

func (c *publicCache) put(q string, rs []discovery.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]publicEntry{}
	}
	if len(c.items) >= publicCacheCap {
		// Drop the oldest entry.
		var oldest string
		for k, e := range c.items {
			if oldest == "" || e.at.Before(c.items[oldest].at) {
				oldest = k
			}
		}
		delete(c.items, oldest)
	}
	c.items[q] = publicEntry{rs, time.Now()}
}

// publicResults searches pkg.go.dev, giving up after publicTimeout so a
// slow upstream costs at most that much.
func (s *server) publicResults(ctx context.Context, q string) ([]discovery.Result, error) {
	if rs, ok := s.pubCache.get(q); ok {
		return rs, nil
	}
	ctx, cancel := context.WithTimeout(ctx, publicTimeout)
	defer cancel()
	rs, err := s.disc.Index.Search(ctx, q, unifiedLimit)
	if err != nil {
		return nil, err
	}
	s.pubCache.put(q, rs)
	return rs, nil
}

// unifiedRow is one result of the combined list.
type unifiedRow struct {
	ranking.Candidate
	Hit *registry.SearchHit // for hosted modules
}

// unifiedSearch ranks hosted and public modules together.
func (s *server) unifiedSearch(ctx context.Context, data *searchData) error {
	hits, total, err := s.registry.Search(ctx, registry.SearchQuery{Text: data.Query, Sort: registry.SortRelevance, Limit: hostedPool})
	if err != nil {
		return err
	}
	data.HostedTotal = total
	signals, err := s.registry.Signals(ctx, hits)
	if err != nil {
		return err
	}
	byPath := map[string]*registry.SearchHit{}
	var hosted []ranking.Candidate
	for i := range hits {
		h := &hits[i]
		byPath[h.Path] = h
		sig := signals[h.Path]
		hosted = append(hosted, ranking.Candidate{
			Path: h.Path, Version: h.Version, Synopsis: h.Synopsis, Rank: i, Of: len(hits),
			Downloads30: h.Downloads30, UsedBy: sig.UsedBy, Verified: sig.Verified, Deprecated: h.Deprecated, Vulnerable: sig.Vulnerable,
		})
	}

	var public []ranking.Candidate
	if rs, err := s.publicResults(ctx, data.Query); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Warn("public search", "query", data.Query, "err", err)
		data.PublicError = "Public modules from pkg.go.dev didn't load in time, so only modules on Gopherdex are shown. Search again to retry."
	} else {
		for i, r := range rs {
			public = append(public, ranking.Candidate{Path: r.Path, Version: r.Version, Synopsis: r.Synopsis, Rank: i, Of: len(rs)})
		}
	}

	for _, c := range ranking.Merge(data.Query, hosted, public, unifiedLimit) {
		row := unifiedRow{Candidate: c}
		if c.Hosted {
			row.Hit = byPath[c.Path]
			data.HostedShown++
		} else {
			data.PublicShown++
		}
		data.Results = append(data.Results, row)
	}
	return nil
}
