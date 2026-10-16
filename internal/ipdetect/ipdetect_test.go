package ipdetect

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// quiet logger for tests
func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseIP(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{name: "bare", body: "203.0.113.7", want: "203.0.113.7"},
		{name: "trailing newline (icanhazip)", body: "203.0.113.7\n", want: "203.0.113.7"},
		{name: "surrounding whitespace", body: "  203.0.113.7 \n", want: "203.0.113.7"},
		{name: "ipv6", body: "2001:db8::1", want: "2001:db8::1"},
		{name: "ipv6 with newline", body: "2001:db8::1\r\n", want: "2001:db8::1"},
		{name: "json", body: `{"ip":"203.0.113.7"}`, want: "203.0.113.7"},
		{name: "empty", body: "", wantErr: true},
		{name: "whitespace only", body: "   \n", wantErr: true},
		{name: "html error page", body: "<html><body>nope</body></html>", wantErr: true},
		{name: "not an ip", body: "hello world", wantErr: true},
		{name: "ip with port", body: "203.0.113.7:8080", wantErr: true},
		{name: "partial ip", body: "203.0.113", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseIP(tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseIP(%q) = %q, want error", tc.body, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIP(%q) unexpected error: %v", tc.body, err)
			}
			if got != tc.want {
				t.Fatalf("ParseIP(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// echoServer returns a provider URL that always answers with body.
func echoServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDetectConsensus(t *testing.T) {
	agreeA := echoServer(t, "203.0.113.7\n")
	agreeB := echoServer(t, "203.0.113.7")
	liar := echoServer(t, "198.51.100.99\n")

	d := New(Config{Providers: []string{agreeA, agreeB, liar}, Timeout: 2 * time.Second}, testLog())
	got, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got != "203.0.113.7" {
		t.Fatalf("Detect = %q, want 203.0.113.7 (majority answer)", got)
	}
}

func TestDetectSingleProvider(t *testing.T) {
	d := New(Config{Providers: []string{echoServer(t, "203.0.113.7\n")}, Timeout: 2 * time.Second}, testLog())
	got, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got != "203.0.113.7" {
		t.Fatalf("Detect = %q, want 203.0.113.7", got)
	}
}

func TestDetectAllProvidersFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	d := New(Config{Providers: []string{srv.URL, "http://127.0.0.1:1/"}, Timeout: time.Second}, testLog())
	if _, err := d.Detect(context.Background()); err == nil {
		t.Fatal("Detect: want error when every provider fails")
	}
}

func TestDetectInvalidResponsesIgnored(t *testing.T) {
	garbage := echoServer(t, "<html>maintenance</html>")
	good := echoServer(t, "203.0.113.7\n")

	d := New(Config{Providers: []string{garbage, good}, Timeout: 2 * time.Second}, testLog())
	got, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got != "203.0.113.7" {
		t.Fatalf("Detect = %q, want the only valid answer 203.0.113.7", got)
	}
}

func TestRunReportsChangesOnly(t *testing.T) {
	var calls atomic.Int64
	body := atomic.Value{}
	body.Store("203.0.113.7\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	t.Cleanup(srv.Close)

	d := New(Config{
		Providers: []string{srv.URL},
		Interval:  20 * time.Millisecond,
		Timeout:   time.Second,
	}, testLog())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan string, 8)
	go func() {
		_ = d.Run(ctx, func(ip string) { changes <- ip })
	}()

	// First observation always fires.
	select {
	case ip := <-changes:
		if ip != "203.0.113.7" {
			t.Fatalf("first change = %q, want 203.0.113.7", ip)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the initial IP")
	}

	// Give it a few rounds with the same answer; nothing more should fire.
	time.Sleep(120 * time.Millisecond)
	select {
	case ip := <-changes:
		t.Fatalf("unexpected second change for an unchanged IP: %q", ip)
	default:
	}

	// Now the "IP changes"; a new event must arrive.
	body.Store("203.0.113.99\n")
	select {
	case ip := <-changes:
		if ip != "203.0.113.99" {
			t.Fatalf("change = %q, want 203.0.113.99", ip)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the IP change")
	}

	if calls.Load() < 3 {
		t.Fatalf("provider called %d times, expected several rounds", calls.Load())
	}
}

func TestRunKeepsGoingWhenProvidersFail(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, "203.0.113.7\n")
	}))
	t.Cleanup(srv.Close)

	d := New(Config{
		Providers: []string{srv.URL},
		Interval:  20 * time.Millisecond,
		Timeout:   time.Second,
	}, testLog())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := make(chan string, 1)
	go func() {
		_ = d.Run(ctx, func(ip string) { changes <- ip })
	}()

	// Providers are down: no event, and Run must not return an error/exit.
	time.Sleep(100 * time.Millisecond)
	up.Store(true)

	select {
	case ip := <-changes:
		if ip != "203.0.113.7" {
			t.Fatalf("change = %q, want 203.0.113.7", ip)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("detector did not recover after providers came back")
	}
}

func TestDefaultProvidersUsedWhenUnset(t *testing.T) {
	d := New(Config{}, testLog())
	if len(d.cfg.Providers) != len(DefaultProviders) {
		t.Fatalf("providers = %d, want default %d", len(d.cfg.Providers), len(DefaultProviders))
	}
	if d.cfg.Interval != 60*time.Second {
		t.Fatalf("interval = %v, want 60s default", d.cfg.Interval)
	}
	if d.cfg.Timeout != 10*time.Second {
		t.Fatalf("timeout = %v, want 10s default", d.cfg.Timeout)
	}
}

func TestDetectHonoursContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the client gives up
	}))
	t.Cleanup(srv.Close)

	d := New(Config{Providers: []string{srv.URL}, Timeout: 50 * time.Millisecond}, testLog())
	_, err := d.Detect(context.Background())
	if err == nil {
		t.Fatal("Detect: want an error from a hanging provider")
	}
	if !strings.Contains(err.Error(), "providers failed") {
		t.Fatalf("Detect error = %v, want a providers-failed error", err)
	}
}
