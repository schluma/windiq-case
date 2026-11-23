package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// apiClient talks to the WindIQ measurement API. Only the ingest goroutine
// uses it, so it is not safe for concurrent use.
type apiClient struct {
	baseURL      string
	clientID     string
	clientSecret string
	http         *http.Client

	token       string
	tokenExpiry time.Time
}

func newAPIClient(baseURL, clientID, clientSecret string) *apiClient {
	return &apiClient{
		baseURL:      strings.TrimRight(baseURL, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         &http.Client{Timeout: 20 * time.Second},
	}
}

// apiError is an answer from the API with a status code other than 2xx.
type apiError struct {
	Status     int
	Body       string
	RetryAfter time.Duration // from the Retry-After header, 0 if missing
}

func (e *apiError) Error() string {
	return fmt.Sprintf("api status %d: %s", e.Status, e.Body)
}

type mast struct {
	ID                 string  `json:"id"`
	MeasurementHeightM float64 `json:"measurement_height_m"`
}

type powerCurvePoint struct {
	WindSpeedMS float64 `json:"wind_speed_ms"`
	PowerKW     float64 `json:"power_kw"`
}

type turbine struct {
	ID         string            `json:"id"`
	HubHeightM float64           `json:"hub_height_m"`
	PowerCurve []powerCurvePoint `json:"power_curve"`
	Version    int               `json:"version"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

type losses struct {
	Availability float64 `json:"availability"`
	Electrical   float64 `json:"electrical"`
	Wake         float64 `json:"wake"`
}

type site struct {
	ID            string    `json:"id"`
	ShearExponent float64   `json:"shear_exponent"`
	Losses        losses    `json:"losses"`
	Uncertainty   float64   `json:"uncertainty"`
	Version       int       `json:"version"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// rawMeasurement is a measurement exactly as the API delivers it.
// wind_speed may be a number, a string or null.
type rawMeasurement struct {
	Timestamp string          `json:"timestamp"`
	WindSpeed json.RawMessage `json:"wind_speed"`
	Unit      string          `json:"unit"`
	Version   int             `json:"version"`
	UpdatedAt string          `json:"updated_at"`
}

type measurementPage struct {
	Data       []rawMeasurement `json:"data"`
	NextCursor *string          `json:"next_cursor"`
}

func (c *apiClient) fetchMast(ctx context.Context) (mast, error) {
	var m mast
	err := c.get(ctx, "/v1/mast", nil, &m)
	return m, err
}

func (c *apiClient) fetchTurbine(ctx context.Context) (turbine, error) {
	var t turbine
	err := c.get(ctx, "/v1/turbine", nil, &t)
	return t, err
}

func (c *apiClient) fetchSite(ctx context.Context) (site, error) {
	var s site
	err := c.get(ctx, "/v1/site", nil, &s)
	return s, err
}

// fetchMeasurements returns all measurements with updated_at >= updatedSince,
// or all available ones when updatedSince is zero. It follows the cursor
// until the last page.
func (c *apiClient) fetchMeasurements(ctx context.Context, updatedSince time.Time) ([]rawMeasurement, error) {
	query := url.Values{}
	query.Set("limit", "500")
	if !updatedSince.IsZero() {
		query.Set("updated_since", updatedSince.UTC().Format(time.RFC3339))
	}

	var all []rawMeasurement
	// The limit only guards against a cursor that never ends.
	for range 1000 {
		var page measurementPage
		err := c.get(ctx, "/v1/measurements", query, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return all, nil
		}
		query.Set("cursor", *page.NextCursor)
	}
	return nil, errors.New("too many pages")
}

// get calls an authorized GET endpoint and decodes the JSON answer into out.
// On 401 it renews the token once and tries again.
func (c *apiClient) get(ctx context.Context, path string, query url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		err := c.ensureToken(ctx)
		if err != nil {
			return err
		}

		u := c.baseURL + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)

		err = c.do(req, out)
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized && attempt == 0 {
			c.token = ""
			continue
		}
		return err
	}
}

// ensureToken requests a new access token when there is none or it expires
// within the next minute.
func (c *apiClient) ensureToken(ctx context.Context) error {
	if c.token != "" && time.Until(c.tokenExpiry) > time.Minute {
		return nil
	}

	body, err := json.Marshal(map[string]string{
		"client_id":     c.clientID,
		"client_secret": c.clientSecret,
		"grant_type":    "client_credentials",
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/auth/token", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	err = c.do(req, &tok)
	if err != nil {
		return fmt.Errorf("request token: %w", err)
	}
	if tok.AccessToken == "" {
		return errors.New("request token: empty access_token")
	}
	c.token = tok.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return nil
}

func (c *apiClient) do(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{
			Status:     resp.StatusCode,
			Body:       shorten(string(body), 200),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	err = json.Unmarshal(body, out)
	if err != nil {
		return fmt.Errorf("decode %s: %w", req.URL.Path, err)
	}
	return nil
}

// parseRetryAfter understands the seconds form of Retry-After.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
