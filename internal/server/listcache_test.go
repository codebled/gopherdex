package server

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCached(t *testing.T) {
	loads := 0
	load := func() (int, error) { loads++; return loads, nil }

	off := cached[int]{}
	off.get(load)
	if v, _ := off.get(load); v != 2 {
		t.Errorf("zero TTL should load every time, got %d", v)
	}

	loads = 0
	c := cached[int]{TTL: time.Hour}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); c.get(load) }()
	}
	wg.Wait()
	if loads != 1 {
		t.Errorf("%d loads for 20 concurrent requests, want 1", loads)
	}

	// Errors aren't cached.
	bad := cached[int]{TTL: time.Hour}
	if _, err := bad.get(func() (int, error) { return 0, errors.New("db down") }); err == nil {
		t.Fatal("want the error")
	}
	if v, _ := bad.get(load); v != 2 {
		t.Errorf("after an error the next call should load, got %d", v)
	}
}
