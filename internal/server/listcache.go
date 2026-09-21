package server

import (
	"sync"
	"time"
)

// cached holds a value that is expensive to compute and fine to serve a
// little stale, such as the home page's listings: each needs a pass over
// every module. Concurrent callers wait for one load instead of each
// running it. A zero TTL turns caching off.
type cached[T any] struct {
	TTL time.Duration

	mu  sync.Mutex
	at  time.Time
	val T
}

func (c *cached[T]) get(load func() (T, error)) (T, error) {
	if c.TTL <= 0 {
		return load()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < c.TTL {
		return c.val, nil
	}
	v, err := load()
	if err != nil {
		return v, err
	}
	c.val, c.at = v, time.Now()
	return v, nil
}
