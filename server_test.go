package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHandler(newest time.Time) http.Handler {
	s := &server{maxAge: 3 * time.Hour, newestMeasurement: newest}
	return s.routes()
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func decodeHealth(t *testing.T, rec *httptest.ResponseRecorder) healthResponse {
	t.Helper()
	var body healthResponse
	err := json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return body
}

func TestLivezAlwaysOK(t *testing.T) {
	// Even without any data the process counts as alive.
	rec := do(t, newTestHandler(time.Time{}), http.MethodGet, "/livez")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	tests := []struct {
		name       string
		newest     time.Time
		wantCode   int
		wantStatus healthStatus
		wantAge    int64 // seconds, ignored when there is no data
	}{
		{"no data", time.Time{}, http.StatusServiceUnavailable, healthNoData, 0},
		{"fresh", time.Now().Add(-90 * time.Minute), http.StatusOK, healthOK, 5400},
		{"stale", time.Now().Add(-4 * time.Hour), http.StatusServiceUnavailable, healthStale, 14400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, newTestHandler(tc.newest), http.MethodGet, "/health")
			if rec.Code != tc.wantCode {
				t.Errorf("code = %d, want %d", rec.Code, tc.wantCode)
			}
			cacheControl := rec.Header().Get("Cache-Control")
			if cacheControl != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cacheControl)
			}

			body := decodeHealth(t, rec)
			if body.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", body.Status, tc.wantStatus)
			}

			if tc.newest.IsZero() {
				if body.DataAgeSeconds != nil {
					t.Errorf("data_age_seconds = %d, want none", *body.DataAgeSeconds)
				}
				return
			}
			// A few seconds of slack: the test itself takes time.
			if body.DataAgeSeconds == nil || *body.DataAgeSeconds < tc.wantAge || *body.DataAgeSeconds > tc.wantAge+10 {
				t.Errorf("data_age_seconds = %v, want about %d", body.DataAgeSeconds, tc.wantAge)
			}
		})
	}
}

func TestIndex(t *testing.T) {
	t.Run("without data", func(t *testing.T) {
		rec := do(t, newTestHandler(time.Time{}), http.MethodGet, "/")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Noch keine Messdaten") {
			t.Error("expected placeholder text")
		}
	})
	t.Run("with data", func(t *testing.T) {
		newest := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
		rec := do(t, newTestHandler(newest), http.MethodGet, "/")
		if !strings.Contains(rec.Body.String(), "2026-11-23 12:00 UTC") {
			t.Errorf("expected newest timestamp in body, got:\n%s", rec.Body.String())
		}
	})
}

func TestRouting(t *testing.T) {
	h := newTestHandler(time.Time{})
	rec := do(t, h, http.MethodGet, "/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: status = %d, want 404", rec.Code)
	}
	rec = do(t, h, http.MethodPost, "/livez")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /livez: status = %d, want 405", rec.Code)
	}
}

func TestFavicon(t *testing.T) {
	rec := do(t, newTestHandler(time.Time{}), http.MethodGet, "/favicon.ico")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	contentType := rec.Header().Get("Content-Type")
	if contentType != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", contentType)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Error("expected SVG body")
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := do(t, newTestHandler(time.Time{}), http.MethodGet, "/")
	nosniff := rec.Header().Get("X-Content-Type-Options")
	if nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", nosniff)
	}
}
