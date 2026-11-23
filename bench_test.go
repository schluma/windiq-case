package main

import (
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// benchHandler builds the server with a realistic year of data: 8760 hours,
// varying wind, a real power curve.
func benchHandler(b *testing.B) http.Handler {
	b.Helper()
	// Request logs would dominate the measurement and flood the output.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(previous) })

	newest := time.Now().UTC().Truncate(time.Hour)
	i := 0
	ms := fullYear(newest, func(h time.Time) float64 {
		i++
		return math.Max(0, 5.5+2.5*math.Sin(float64(i)/37)+1.2*math.Sin(float64(i)/5.3))
	})
	curve := []powerCurvePoint{{0, 0}, {3, 55}, {5, 385}, {8, 1590}, {11, 3250}, {14, 3600}, {25, 3600}}
	st := newStore()
	st.setMasterData(&mast{MeasurementHeightM: 100}, &turbine{HubHeightM: 140, PowerCurve: curve},
		&site{ShearExponent: 0.2, Losses: losses{0.03, 0.02, 0.05}, Uncertainty: 0.1})
	st.upsert(ms)
	return (&server{maxAge: 3 * time.Hour, store: st}).routes()
}

func benchmarkPath(b *testing.B, path string) {
	h := benchHandler(b)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.ServeHTTP(httptest.NewRecorder(), req) // fills the result cache
	b.ResetTimer()
	for range b.N {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d", rec.Code)
		}
	}
}

func BenchmarkIndex(b *testing.B)  { benchmarkPath(b, "/") }
func BenchmarkHealth(b *testing.B) { benchmarkPath(b, "/health") }

// BenchmarkIndexParallel calls the page from all cores at once, to show that
// the store's locks do not serialize the readers.
func BenchmarkIndexParallel(b *testing.B) {
	h := benchHandler(b)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != http.StatusOK {
				b.Errorf("status %d", rec.Code)
				return
			}
		}
	})
}
