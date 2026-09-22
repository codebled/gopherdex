package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(2, time.Minute)
	l.now = func() time.Time { return now }

	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("first two events should be allowed")
	}
	if l.Allow("a") {
		t.Fatal("third event in the window should be blocked")
	}
	if !l.Allow("b") {
		t.Fatal("keys are independent")
	}
	now = now.Add(time.Minute)
	if !l.Allow("a") {
		t.Fatal("a new window should reset the count")
	}
}

func TestLimiterUnderKeyFlood(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := New(5, time.Minute)
	l.now = func() time.Time { return now }
	for i := range maxKeys {
		l.Allow(fmt.Sprint(i))
	}
	// Full of live buckets: known keys still count, new keys are refused.
	if !l.Allow("0") {
		t.Error("an existing key should still be counted")
	}
	if l.Allow("newcomer") {
		t.Error("a new key was accepted with the map full")
	}
	// Sweeps run at most once a second, and clear expired buckets.
	now = now.Add(time.Minute)
	if !l.Allow("newcomer") || len(l.buckets) != 1 {
		t.Errorf("after the window: %d buckets", len(l.buckets))
	}
	for i := range sweepAbove + 10 {
		l.Allow(fmt.Sprint("x", i))
	}
	swept := l.lastSweep
	now = now.Add(time.Millisecond)
	l.Allow("y")
	if l.lastSweep != swept {
		t.Error("swept twice within a second")
	}
}
