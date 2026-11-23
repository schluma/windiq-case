package main

import (
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a token bucket shared by all clients: up to burst requests
// at once, refilled with rate requests per second.
//
// It is deliberately global and not per client IP. Behind Render's proxy the
// client IP comes from X-Forwarded-For, which a client can partly forge, and
// many users may share one proxy address. A global limit needs none of that:
// it caps the total load, so the app stays responsive under a flood and the
// ingest keeps its CPU. The limit lies far above what a dashboard needs.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time

	rejected       int // since the last log line
	lastRejectsLog time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// allow takes one token if there is one.
func (l *rateLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		return true
	}

	// Rejected requests are not logged one by one, that would flood the log
	// exactly when it matters. One summary line per minute instead.
	l.rejected++
	if now.Sub(l.lastRejectsLog) >= time.Minute {
		slog.Warn("rate limit exceeded", "rejected_since_last_report", l.rejected, "limit_per_second", l.rate)
		l.rejected = 0
		l.lastRejectsLog = now
	}
	return false
}

// limitRequests answers 429 when the limit is exceeded. /livez and /health
// are exempt: Render and the uptime monitor must always get an honest
// answer, also during a flood.
func limitRequests(l *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/livez" || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if !l.allow(time.Now()) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
