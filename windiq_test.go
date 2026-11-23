package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeAPI imitates the WindIQ API for tests.
type fakeAPI struct {
	mu sync.Mutex

	measurements []map[string]any // returned as is, so tests can send odd values
	pageSize     int
	mast         map[string]any
	turbine      map[string]any
	site         map[string]any

	validToken    string
	tokenRequests int
	failNext      []int    // status codes for the next /v1/ requests
	lastQueries   []string // raw query of each /v1/measurements request
}

func newFakeAPI(t *testing.T) (*fakeAPI, *apiClient) {
	t.Helper()
	f := &fakeAPI{
		pageSize: 2,
		mast:     map[string]any{"id": "WIQ-MM-01", "measurement_height_m": 100},
		turbine: map[string]any{
			"id":           "WIQ-T36",
			"hub_height_m": 140,
			"power_curve": []map[string]any{
				{"wind_speed_ms": 0, "power_kw": 0},
				{"wind_speed_ms": 3, "power_kw": 55},
				{"wind_speed_ms": 13, "power_kw": 3600},
				{"wind_speed_ms": 25, "power_kw": 3600},
			},
			"version":    1,
			"updated_at": "2026-09-20T08:00:00Z",
		},
		site: map[string]any{
			"id":             "WIQ-SITE-01",
			"shear_exponent": 0.2,
			"losses":         map[string]any{"availability": 0.03, "electrical": 0.02, "wake": 0.05},
			"uncertainty":    0.1,
			"version":        1,
			"updated_at":     "2026-09-20T08:00:00Z",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, newAPIClient(srv.URL, "id", "secret")
}

func rec(timestamp string, speed any, unit string, version int, updatedAt string) map[string]any {
	return map[string]any{
		"timestamp":          timestamp,
		"wind_speed":         speed,
		"unit":               unit,
		"wind_direction_deg": 240,
		"version":            version,
		"updated_at":         updatedAt,
	}
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/auth/token" {
		var body map[string]string
		err := json.NewDecoder(r.Body).Decode(&body)
		if err != nil || body["client_id"] != "id" || body["client_secret"] != "secret" {
			http.Error(w, `{"detail":"invalid client"}`, http.StatusUnauthorized)
			return
		}
		f.tokenRequests++
		f.validToken = fmt.Sprintf("tok-%d", f.tokenRequests)
		writeJSON(w, map[string]any{"access_token": f.validToken, "token_type": "Bearer", "expires_in": 3600})
		return
	}

	if len(f.failNext) > 0 {
		code := f.failNext[0]
		f.failNext = f.failNext[1:]
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}
		http.Error(w, `{"detail":"injected failure"}`, code)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.validToken {
		http.Error(w, `{"detail":"invalid token"}`, http.StatusUnauthorized)
		return
	}

	switch r.URL.Path {
	case "/v1/mast":
		writeJSON(w, f.mast)
	case "/v1/turbine":
		writeJSON(w, f.turbine)
	case "/v1/site":
		writeJSON(w, f.site)
	case "/v1/measurements":
		f.lastQueries = append(f.lastQueries, r.URL.RawQuery)
		f.serveMeasurements(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) serveMeasurements(w http.ResponseWriter, r *http.Request) {
	matching := f.measurements
	since := r.URL.Query().Get("updated_since")
	if since != "" {
		sinceTime, err := time.Parse(time.RFC3339, since)
		if err != nil {
			http.Error(w, `{"detail":"bad updated_since"}`, http.StatusBadRequest)
			return
		}
		matching = nil
		for _, m := range f.measurements {
			updated, _ := time.Parse(time.RFC3339, m["updated_at"].(string))
			if !updated.Before(sinceTime) {
				matching = append(matching, m)
			}
		}
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	end := min(offset+f.pageSize, len(matching))
	page := map[string]any{"data": matching[offset:end], "next_cursor": nil}
	if end < len(matching) {
		page["next_cursor"] = strconv.Itoa(end)
	}
	writeJSON(w, page)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestClientReusesToken(t *testing.T) {
	f, c := newFakeAPI(t)
	for range 3 {
		_, err := c.fetchMast(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.tokenRequests != 1 {
		t.Errorf("token requests = %d, want 1", f.tokenRequests)
	}
}

func TestClientRenewsTokenOn401(t *testing.T) {
	f, c := newFakeAPI(t)
	_, err := c.fetchMast(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The API no longer accepts the token, e.g. after a restart on its side.
	f.mu.Lock()
	f.validToken = "revoked"
	f.mu.Unlock()

	m, err := c.fetchMast(context.Background())
	if err != nil {
		t.Fatalf("expected renewal to succeed, got %v", err)
	}
	if m.MeasurementHeightM != 100 {
		t.Errorf("height = %v", m.MeasurementHeightM)
	}
	if f.tokenRequests != 2 {
		t.Errorf("token requests = %d, want 2", f.tokenRequests)
	}
}

func TestClientWrongCredentials(t *testing.T) {
	_, c := newFakeAPI(t)
	c.clientSecret = "wrong"
	_, err := c.fetchMast(context.Background())
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want api status 401", err)
	}
}

func TestFetchMeasurementsFollowsCursor(t *testing.T) {
	f, c := newFakeAPI(t)
	for i := range 5 {
		ts := time.Date(2026, 11, 1, i, 0, 0, 0, time.UTC).Format(time.RFC3339)
		f.measurements = append(f.measurements, rec(ts, 5.0, "m/s", 1, ts))
	}

	got, err := c.fetchMeasurements(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("got %d measurements, want 5", len(got))
	}
	if len(f.lastQueries) != 3 {
		t.Errorf("got %d page requests, want 3", len(f.lastQueries))
	}
}

func TestAPIErrorCarriesRetryAfter(t *testing.T) {
	f, c := newFakeAPI(t)
	f.failNext = []int{http.StatusTooManyRequests}

	_, err := c.fetchMast(context.Background())
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *apiError", err)
	}
	if apiErr.Status != http.StatusTooManyRequests || apiErr.RetryAfter != 7*time.Second {
		t.Errorf("got status %d, retry after %v", apiErr.Status, apiErr.RetryAfter)
	}
}
