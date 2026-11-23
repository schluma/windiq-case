// Command windiq-case fetches hourly met-mast data, computes AEP, P50 and
// P90 and serves them as a small web dashboard.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	err := run()
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Render sends SIGTERM on every deploy; we stop gracefully on it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	port := envOr("PORT", "8080")
	maxAge, err := time.ParseDuration(envOr("MAX_DATA_AGE", "3h"))
	if err != nil {
		return err
	}
	pollInterval, err := time.ParseDuration(envOr("POLL_INTERVAL", "5m"))
	if err != nil {
		return err
	}
	maxInvalidations, err := strconv.Atoi(envOr("MAX_INVALIDATIONS_PER_POLL", "24"))
	if err != nil {
		return fmt.Errorf("MAX_INVALIDATIONS_PER_POLL: %w", err)
	}
	rateLimit, err := strconv.ParseFloat(envOr("RATE_LIMIT_PER_SECOND", "200"), 64)
	if err != nil {
		return fmt.Errorf("RATE_LIMIT_PER_SECOND: %w", err)
	}
	apiURL := envOr("WINDIQ_API_URL", "https://windiq-mock-api.sliplane.app")
	snapshotPath := envOr("SNAPSHOT_PATH", "data/snapshot.json")
	clientID := os.Getenv("WINDIQ_CLIENT_ID")
	clientSecret := os.Getenv("WINDIQ_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return errors.New("WINDIQ_CLIENT_ID and WINDIQ_CLIENT_SECRET must be set")
	}

	st := newStore()
	snap, outdated, err := loadSnapshot(snapshotPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		slog.Info("no snapshot yet, starting empty", "path", snapshotPath)
	case err != nil:
		// A broken snapshot must not stop the app; the first poll refills everything.
		slog.Error("snapshot unusable, starting empty", "path", snapshotPath, "err", err)
	case outdated:
		st.restore(snap)
		slog.Warn("snapshot has an old format, showing it until a full reload succeeds",
			"path", snapshotPath, "format", snap.FormatVersion, "current", snapshotFormatVersion,
			"measurements", len(snap.Measurements))
	default:
		st.restore(snap)
		slog.Info("snapshot loaded", "path", snapshotPath, "measurements", len(snap.Measurements), "saved_at", snap.SavedAt)
	}

	in := &ingester{
		client:           newAPIClient(apiURL, clientID, clientSecret),
		store:            st,
		snapshotPath:     snapshotPath,
		interval:         pollInterval,
		maxInvalidations: maxInvalidations,
		fullReload:       outdated,
	}
	ingestDone := make(chan struct{})
	go func() {
		in.run(ctx)
		close(ingestDone)
	}()

	// Bursts of up to two seconds' worth of requests are allowed.
	s := &server{maxAge: maxAge, store: st, limiter: newRateLimiter(rateLimit, 2*rateLimit)}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: s.routes(),
		// Explicit timeouts protect against slow or hanging clients.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	// Wait until the ingest goroutine has stopped (it stops when ctx is
	// cancelled), so that a snapshot being written is finished.
	<-ingestDone
	return err
}

func envOr(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}
