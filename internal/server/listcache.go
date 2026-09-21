package server

import (
	"context"
	"sync"
	"time"
)

// cached holds a value that is expensive to compute and fine to serve a
// little stale, such as the home page's listings: each needs a pass over
// every module. The first request loads it (others wait for that load);
// after that, a request that finds it older than TTL still gets the cached
// value at once and one background refresh replaces it. A zero TTL turns
// caching off.
type cached[T any] struct {
	TTL time.Duration

	mu         sync.Mutex
	has        bool
	at         time.Time
	val        T
	refreshing bool
}

func (c *cached[T]) get(ctx context.Context, load func(context.Context) (T, error)) (T, error) {
	if c.TTL <= 0 {
		return load(ctx)
	}
	c.mu.Lock()
	if c.has {
		if time.Since(c.at) >= c.TTL && !c.refreshing {
			c.refreshing = true
			go c.refresh(load)
		}
		v := c.val
		c.mu.Unlock()
		return v, nil
	}
	defer c.mu.Unlock()
	v, err := load(ctx)
	if err != nil {
		return v, err
	}
	c.val, c.at, c.has = v, time.Now(), true
	return v, nil
}

// refresh reloads the value in the background. On failure it keeps the old
// value; the next request after TTL tries again.
func (c *cached[T]) refresh(load func(context.Context) (T, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	v, err := load(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	if err == nil {
		c.val, c.at = v, time.Now()
	}
}
