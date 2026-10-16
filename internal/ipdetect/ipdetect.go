// Package ipdetect determines this machine's current public IP address by
// asking one or more "what is my IP" echo services, and reports changes over
// time. It is what replaced the old standalone beacon process: the coordinator
// now runs on the laptop and learns its own address outbound, so nothing has
// to be installed on the laptop apart from the coordinator itself.
package ipdetect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultProviders is the built-in set of echo services. They are plain-text
// endpoints that return the caller's public IP as the response body. They are
// queried in parallel and the most-reported answer wins, so any single one
// going away (or lying) is not fatal.
var DefaultProviders = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
	"https://ipinfo.io/ip",
	"https://checkip.amazonaws.com",
}

// maxBody caps how much of an echo response we read.
const maxBody = 4 << 10 // 4 KiB is far more than any plausible response

// Config configures a Detector.
type Config struct {
	// Providers are URLs returning the caller's IP as plain text. Empty uses
	// DefaultProviders.
	Providers []string
	// Interval between detection rounds. Default 60s.
	Interval time.Duration
	// Timeout for one provider request. Default 10s.
	Timeout time.Duration
	// HTTPClient overrides the HTTP client (e.g. for tests).
	HTTPClient *http.Client
}

// Detector periodically resolves the public IP.
type Detector struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger
}

// New builds a Detector.
func New(cfg Config, log *slog.Logger) *Detector {
	if len(cfg.Providers) == 0 {
		cfg.Providers = append([]string(nil), DefaultProviders...)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		// No client-level timeout: each request gets one from the round's
		// context, so a slow provider cannot stall the whole round past the
		// per-provider deadline.
		hc = &http.Client{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Detector{cfg: cfg, http: hc, log: log}
}

// Run blocks until ctx is cancelled, invoking onChange whenever the observed
// IP differs from the previous round's. The first successful observation always
// fires onChange, so a freshly started coordinator records its IP at once
// rather than waiting for a change.
func (d *Detector) Run(ctx context.Context, onChange func(ip string)) error {
	last := ""

	round := func() {
		ip, err := d.Detect(ctx)
		if err != nil {
			// Not fatal: a laptop on a train, a flaky DNS, an outage at the
			// echo services — all transient. The last known IP stays applied.
			d.log.Warn("public IP detection failed", "err", err)
			return
		}
		if ip == last {
			d.log.Debug("public IP unchanged", "ip", ip)
			return
		}
		d.log.Info("public IP detected", "ip", ip)
		last = ip
		if onChange != nil {
			onChange(ip)
		}
	}

	round()

	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			round()
		}
	}
}

// Detect performs one detection round and returns the most-reported public IP.
// It returns an error when no provider answered with a parseable address.
func (d *Detector) Detect(ctx context.Context) (string, error) {
	if len(d.cfg.Providers) == 0 {
		return "", errors.New("no IP echo providers configured")
	}

	type result struct{ ip string }
	results := make(chan result, len(d.cfg.Providers))

	ctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, u := range d.cfg.Providers {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			ip, err := d.fetch(ctx, url)
			if err != nil {
				d.log.Debug("ip echo provider failed", "url", url, "err", err)
				return
			}
			results <- result{ip: ip}
		}(u)
	}
	wg.Wait()
	close(results)

	votes := map[string]int{}
	order := []string{}
	total := 0
	for r := range results {
		if _, seen := votes[r.ip]; !seen {
			order = append(order, r.ip)
		}
		votes[r.ip]++
		total++
	}
	if total == 0 {
		return "", fmt.Errorf("all %d IP echo providers failed", len(d.cfg.Providers))
	}

	// Winner = most votes; ties broken by provider order so the result is
	// deterministic for a given provider list.
	best, bestVotes := "", 0
	for _, ip := range order {
		if votes[ip] > bestVotes {
			best, bestVotes = ip, votes[ip]
		}
	}
	if total > 1 && bestVotes == 1 && len(votes) > 1 {
		d.log.Warn("ip echo providers disagree", "answers", fmt.Sprint(sortedKeys(votes)))
	}
	return best, nil
}

// fetch asks one echo service for the caller's IP and validates the answer.
func (d *Detector) fetch(ctx context.Context, url string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ipcod")
	req.Header.Set("Accept", "text/plain")

	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", err
	}
	return ParseIP(string(body))
}

// ParseIP extracts a valid IP address from an echo service response. Responses
// differ in shape (bare IP, trailing newline, JSON like {"ip":"1.2.3.4"}),
// so this accepts all of them.
func ParseIP(body string) (string, error) {
	s := strings.TrimSpace(body)
	if s == "" {
		return "", errors.New("empty response")
	}
	if strings.HasPrefix(s, "{") {
		// e.g. https://api.ipify.org?format=json
		if i := strings.Index(s, `"ip"`); i >= 0 {
			rest := s[i+4:]
			if j := strings.IndexByte(rest, '"'); j >= 0 {
				rest = rest[j+1:]
				if k := strings.IndexByte(rest, '"'); k >= 0 {
					s = rest[:k]
				}
			}
		}
	}
	s = strings.TrimSpace(s)
	if net.ParseIP(s) == nil {
		return "", fmt.Errorf("response is not an IP: %q", truncate(s, 64))
	}
	return s, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sortedKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
