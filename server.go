package main

import (
	"embed"
	"encoding/json"
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
}).ParseFS(templateFS, "templates/*.html"))

//go:embed assets/favicon.svg
var faviconSVG []byte

type server struct {
	maxAge time.Duration
	store  *store
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
	// Browsers request /favicon.ico on their own, also for /health.
	mux.HandleFunc("GET /favicon.ico", favicon)
	return logRequests(securityHeaders(mux))
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

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	st := s.store.status()
	resp := healthResponse{
		Status:        healthOK,
		MaxAgeSeconds: int64(s.maxAge.Seconds()),
		LastError:     st.LastError,
	}
	if !st.LastSuccessfulPoll.IsZero() {
		lastPoll := st.LastSuccessfulPoll.UTC()
		resp.LastSuccessfulPoll = &lastPoll
	}
	code := http.StatusOK

	if st.NewestMeasurement.IsZero() {
		resp.Status = healthNoData
		code = http.StatusServiceUnavailable
	} else {
		newest := st.NewestMeasurement.UTC()
		age := time.Since(newest)
		ageSec := int64(age.Seconds())
		resp.NewestMeasurement = &newest
		resp.DataAgeSeconds = &ageSec
		if age > s.maxAge {
			resp.Status = healthStale
			code = http.StatusServiceUnavailable
		}
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
	NewestMeasurement time.Time
	Mode              gapMode
	Result            result
	CalcError         string // set when there is no result yet
	MonthChart        template.HTML
	DayChart          template.HTML
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

	data := indexData{NewestMeasurement: s.store.status().NewestMeasurement, Mode: mode}
	res, err := s.store.result(mode)
	if err != nil {
		data.CalcError = err.Error()
	} else {
		data.Result = res
		data.MonthChart = monthChart(res)
		data.DayChart = dayChart(res)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = templates.ExecuteTemplate(w, "index.html", data)
	if err != nil {
		slog.Error("render index", "err", err)
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
