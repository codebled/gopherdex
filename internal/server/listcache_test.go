package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCached(t *testing.T) {
	ctx := context.Background()
	var loads atomic.Int32
	load := func(context.Context) (int32, error) { return loads.Add(1), nil }

	off := cached[int32]{}
	off.get(ctx, load)
	if v, _ := off.get(ctx, load); v != 2 {
		t.Errorf("zero TTL should load every time, got %d", v)
	}

	loads.Store(0)
	c := cached[int32]{TTL: time.Hour}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); c.get(ctx, load) }()
	}
	wg.Wait()
	if n := loads.Load(); n != 1 {
		t.Errorf("%d loads for 20 concurrent requests, want 1", n)
	}

	// Once stale, requests get the old value at once while one refresh runs.
	loads.Store(0)
	release := make(chan struct{})
	slow := func(context.Context) (int32, error) {
		n := loads.Add(1)
		if n > 1 {
			<-release
		}
		return n, nil
	}
	s := cached[int32]{TTL: time.Millisecond}
	s.get(ctx, slow)
	time.Sleep(2 * time.Millisecond)
	for range 5 {
		if v, _ := s.get(ctx, slow); v != 1 {
			t.Fatalf("stale read = %d, want the cached 1 without waiting", v)
		}
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		v, refreshing := s.val, s.refreshing
		s.mu.Unlock()
		if v == 2 && !refreshing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refresh didn't land: val %d", v)
		}
		time.Sleep(time.Millisecond)
	}
	if n := loads.Load(); n != 2 {
		t.Errorf("%d loads, want 2 (one refresh for five stale reads)", n)
	}

	// Errors aren't cached.
	loads.Store(0)
	bad := cached[int32]{TTL: time.Hour}
	if _, err := bad.get(ctx, func(context.Context) (int32, error) { return 0, errors.New("db down") }); err == nil {
		t.Fatal("want the error")
	}
	if v, _ := bad.get(ctx, load); v != 1 {
		t.Errorf("after an error the next call should load, got %d", v)
	}
}
