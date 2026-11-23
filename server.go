package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"thousands": formatThousands,
	"pct":       func(fraction float64) float64 { return 100 * fraction },
	"ago":       func(t time.Time) string { return formatAgo(time.Since(t)) },
	"duration":  formatAgo,
}).ParseFS(templateFS, "templates/*.html"))

//go:embed assets/favicon.svg
var faviconSVG []byte

type server struct {
	maxAge  time.Duration
	store   *store
	limiter *rateLimiter // nil: no limit
}

// routes returns the HTTP handler with all routes.
//
//   - GET /livez:  process is alive, always 200. Used by Render.
//   - GET /health: data is fresh (200) or stale/missing (503). Used by the uptime monitor.
//   - GET /:       dashboard.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.livez)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /favicon.ico", favicon)

	h := logRequests(securityHeaders(mux))
	// The limiter sits outside the request log, so a flood of rejected
	// requests does not produce one log line each.
	if s.limiter != nil {
		h = limitRequests(s.limiter, h)
	}
	return h
}

func favicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(faviconSVG)
}

func (s *server) livez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// healthStatus is the value of the "status" field in /health.
type healthStatus string

const (
	healthOK     healthStatus = "ok"
	healthStale  healthStatus = "stale"
	healthNoData healthStatus = "no_data"
)

type healthResponse struct {
	Status            healthStatus `json:"status"`
	NewestMeasurement *time.Time   `json:"newest_measurement,omitempty"`
	DataAgeSeconds    *int64       `json:"data_age_seconds,omitempty"`
	MaxAgeSeconds     int64        `json:"max_age_seconds"`
	// LastSuccessfulPoll and LastError describe the ingest; they do not
	// affect the status code, only the data age does.
	LastSuccessfulPoll *time.Time `json:"last_successful_poll,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
}

// freshness rates the data by the age of the newest valid measurement.
// /health and the page use it.
func (s *server) freshness(st ingestStatus) (healthStatus, time.Duration) {
	if st.NewestMeasurement.IsZero() {
		return healthNoData, 0
	}
	age := time.Since(st.NewestMeasurement)
	if age > s.maxAge {
		return healthStale, age
	}
	return healthOK, age
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	st := s.store.status()
	status, age := s.freshness(st)
	resp := healthResponse{
		Status:        status,
		MaxAgeSeconds: int64(s.maxAge.Seconds()),
		LastError:     st.LastError,
	}
	if !st.LastSuccessfulPoll.IsZero() {
		lastPoll := st.LastSuccessfulPoll.UTC()
		resp.LastSuccessfulPoll = &lastPoll
	}
	if status != healthNoData {
		newest := st.NewestMeasurement.UTC()
		ageSec := int64(age.Seconds())
		resp.NewestMeasurement = &newest
		resp.DataAgeSeconds = &ageSec
	}
	code := http.StatusOK
	if status != healthOK {
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	err := json.NewEncoder(w).Encode(resp)
	if err != nil {
		slog.Error("write health response", "err", err)
	}
}

type indexData struct {
	Ingest     ingestStatus
	Freshness  healthStatus
	MaxAge     time.Duration
	Mode       gapMode
	Result     result
	CalcError  string // set when there is no result yet
	MonthChart template.HTML
	DayChart   template.HTML
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	mode := gapMode(r.URL.Query().Get("gaps"))
	if mode == "" {
		mode = gapIgnore
	}
	if mode != gapIgnore && mode != gapScale {
		http.Error(w, "gaps must be ignore or scale", http.StatusBadRequest)
		return
	}

	st := s.store.status()
	freshness, _ := s.freshness(st)
	data := indexData{Ingest: st, Freshness: freshness, MaxAge: s.maxAge, Mode: mode}
	o, err := s.store.outcome(mode)
	if err != nil {
		data.CalcError = err.Error()
	} else {
		data.Result = o.result
		data.MonthChart = o.monthChart
		data.DayChart = o.dayChart
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = templates.ExecuteTemplate(w, "index.html", data)
	if err != nil {
		slog.Error("render index", "err", err)
	}
}

// formatAgo writes a duration the way the page shows it:
// "3 Min.", "2 Std. 5 Min.", "4 Tage".
func formatAgo(d time.Duration) string {
	minutes := int(d.Minutes())
	switch {
	case minutes < 1:
		return "unter 1 Min."
	case minutes < 60:
		return fmt.Sprintf("%d Min.", minutes)
	case minutes < 48*60:
		return fmt.Sprintf("%d Std. %d Min.", minutes/60, minutes%60)
	default:
		return fmt.Sprintf("%d Tage", minutes/(24*60))
	}
}

// securityHeaders sets a few cheap default headers for the browser.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status code so that logRequests can log it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests writes one log line per request. /livez is skipped because
// Render polls it every few seconds and it would drown out everything else.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/livez" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
