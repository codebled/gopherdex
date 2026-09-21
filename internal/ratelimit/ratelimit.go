// Package ratelimit slows down password guessing and sign-up spam with a
// fixed-window counter per key (usually the client IP).
//
// State lives in memory, so limits are per server process. That is enough
// for a single instance; a shared store is needed once there are several.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows at most Max events per key within each Window.
type Limiter struct {
	Max    int
	Window time.Duration

	mu      sync.Mutex
	buckets map[string]bucket
	now     func() time.Time
}

type bucket struct {
	start time.Time
	count int
}

// New returns a Limiter allowing max events per window.
func New(max int, window time.Duration) *Limiter {
	return &Limiter{Max: max, Window: window, buckets: map[string]bucket{}, now: time.Now}
}

// Allow records an event for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.buckets) > 10000 {
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.Window {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[key]
	if now.Sub(b.start) >= l.Window {
		b = bucket{start: now}
	}
	b.count++
	l.buckets[key] = b
	return b.count <= l.Max
}
