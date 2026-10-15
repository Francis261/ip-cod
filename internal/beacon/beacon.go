// Package beacon implements the laptop-side reporter. It periodically POSTs to
// the coordinator so the coordinator can observe the laptop's current public
// IP from the request's source address, and detect changes.
package beacon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Config configures the beacon.
type Config struct {
	// CoordinatorURL is the http(s) URL of the coordinator's /api/beacon endpoint.
	CoordinatorURL string
	// Token is this beacon's bearer token.
	Token string
	// Interval between check-ins. Default 30s.
	Interval time.Duration
	// HTTPClient overrides the HTTP client (e.g. for tests).
	HTTPClient *http.Client
}

// Client is the beacon daemon.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger
}

// NewClient builds a beacon client.
func NewClient(cfg Config, log *slog.Logger) *Client {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{cfg: cfg, http: hc, log: log}
}

// Run blocks until ctx is cancelled, checking in on every interval. The first
// check-in happens immediately so an IP change is noticed as soon as the
// beacon starts.
func (c *Client) Run(ctx context.Context) error {
	// Immediate first report.
	c.report(ctx)

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.report(ctx)
		}
	}
}

// report performs a single check-in.
func (c *Client) report(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.CoordinatorURL, bytes.NewReader(nil))
	if err != nil {
		c.log.Error("build request", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("check-in failed", "err", err)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var body struct {
			IP       string `json:"ip"`
			Changed  bool   `json:"changed"`
			Revision uint64 `json:"revision"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			c.log.Warn("bad response", "err", err)
			return
		}
		if body.Changed {
			c.log.Info("coordinator recorded IP change", "ip", body.IP, "revision", body.Revision)
		} else {
			c.log.Debug("check-in ok", "ip", body.IP)
		}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Auth problems are not transient; surface loudly.
		c.log.Error("beacon rejected by coordinator", "status", resp.StatusCode,
			"hint", "check the beacon token and that the beacon is enabled")
	default:
		c.log.Warn("unexpected status", "status", resp.StatusCode)
	}
}

// ReportOnce runs a single check-in and returns the observed IP. Useful for a
// one-shot CLI mode and for testing.
func ReportOnce(ctx context.Context, cfg Config, log *slog.Logger) (string, bool, error) {
	c := NewClient(cfg, log)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CoordinatorURL, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("coordinator returned %d", resp.StatusCode)
	}
	var body struct {
		IP      string `json:"ip"`
		Changed bool   `json:"changed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false, err
	}
	return body.IP, body.Changed, nil
}
