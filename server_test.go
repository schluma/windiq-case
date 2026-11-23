package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestHandler builds the handler with one valid measurement at newest,
// or no data when newest is zero.
func newTestHandler(newest time.Time) http.Handler {
	st := newStore()
	if !newest.IsZero() {
		st.upsert([]measurement{{Time: newest, SpeedMS: 5, Valid: true, Version: 1}})
	}
	s := &server{maxAge: 3 * time.Hour, store: st}
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

func TestHealthReportsIngestState(t *testing.T) {
	st := newStore()
	st.upsert([]measurement{{Time: time.Now().Add(-time.Hour), SpeedMS: 5, Valid: true, Version: 1}})
	st.recordPoll(errors.New("api status 503: down"), time.Now())
	h := (&server{maxAge: 3 * time.Hour, store: st}).routes()

	rec := do(t, h, http.MethodGet, "/health")
	// An ingest error alone does not make /health fail; only the data age counts.
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	body := decodeHealth(t, rec)
	if body.LastError != "api status 503: down" {
		t.Errorf("last_error = %q", body.LastError)
	}
	if body.LastSuccessfulPoll != nil {
		t.Errorf("last_successful_poll = %v, want none", body.LastSuccessfulPoll)
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

func TestIndexShowsResult(t *testing.T) {
	st := newStore()
	// Losses of 0 % keep the expected MWh simple; 0.0 % must still be listed.
	st.setMasterData(&mast{MeasurementHeightM: 100}, &turbine{HubHeightM: 100, PowerCurve: testCurve, Version: 7}, &site{Uncertainty: 0.1, Version: 9})
	// 2 hours at 10 m/s = 4000 kWh, no losses: P50 = 4 MWh, P90 = 4 * 0.8718.
	now := time.Now().UTC().Truncate(time.Hour)
	st.upsert([]measurement{
		{Time: now, SpeedMS: 10, Valid: true, Version: 1},
		{Time: now.Add(-time.Hour), SpeedMS: 10, Valid: true, Version: 1},
	})
	h := (&server{maxAge: 3 * time.Hour, store: st}).routes()

	body := do(t, h, http.MethodGet, "/").Body.String()
	for _, want := range []string{"4 MWh", "3 MWh", "2 von 8760 Stunden", "Anlage v7, Standort v9"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}

	for _, want := range []string{"<svg", "Bruttoenergie pro Monat", "Unsicherheit U</th><td>10.0 %", "Verlust Abschattung</th><td>0.0 %"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	gross := strings.Index(body, ">AEP brutto<")
	net := strings.Index(body, ">AEP netto (P50)<")
	p90 := strings.Index(body, ">P90<")
	if gross < 0 || !(gross < net && net < p90) {
		t.Errorf("tile order: AEP brutto at %d, AEP netto at %d, P90 at %d", gross, net, p90)
	}

	// Scaled to 8760 hours: every month has fewer than a week of valid hours,
	// so all gaps are filled with the overall mean of 2 MWh per hour.
	body = do(t, h, http.MethodGet, "/?gaps=scale").Body.String()
	if !strings.Contains(body, "17.520 MWh") {
		t.Error("scaled page does not contain 17.520 MWh")
	}
}

func TestIndexRejectsUnknownGapMode(t *testing.T) {
	rec := do(t, newTestHandler(time.Time{}), http.MethodGet, "/?gaps=guess")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
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
