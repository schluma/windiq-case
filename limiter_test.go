package main

import (
	"net/http"
	"testing"
	"time"
)

func TestRateLimiterBucket(t *testing.T) {
	l := newRateLimiter(2, 3) // 2 per second, burst 3
	now := l.last

	for i := range 3 {
		if !l.allow(now) {
			t.Fatalf("request %d of the burst rejected", i+1)
		}
	}
	if l.allow(now) {
		t.Fatal("request beyond the burst allowed")
	}

	// After one second two new tokens are there, not more.
	now = now.Add(time.Second)
	if !l.allow(now) || !l.allow(now) {
		t.Fatal("refilled tokens rejected")
	}
	if l.allow(now) {
		t.Fatal("more tokens than refilled")
	}

	// A long pause refills only up to the burst.
	now = now.Add(time.Hour)
	allowed := 0
	for range 10 {
		if l.allow(now) {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("after a long pause %d allowed, want the burst of 3", allowed)
	}
}

func TestLimitRequests(t *testing.T) {
	st := newStore()
	st.upsert([]measurement{{Time: time.Now().Add(-time.Hour), SpeedMS: 5, Valid: true, Version: 1}})
	limiter := newRateLimiter(0.0001, 1) // one request, then practically nothing
	h := (&server{maxAge: 3 * time.Hour, store: st, limiter: limiter}).routes()

	first := do(t, h, http.MethodGet, "/")
	if first.Code != http.StatusOK {
		t.Fatalf("first request: status %d", first.Code)
	}
	second := do(t, h, http.MethodGet, "/")
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "1" {
		t.Errorf("second request: status %d, Retry-After %q, want 429 and 1", second.Code, second.Header().Get("Retry-After"))
	}

	// Monitoring stays reachable even when the limit is exhausted.
	for _, path := range []string{"/health", "/livez"} {
		rec := do(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, rec.Code)
		}
	}
}
