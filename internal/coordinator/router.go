package coordinator

import (
	"log/slog"
	"net/http"
	"strings"
)

// Config holds coordinator runtime configuration.
type Config struct {
	// ListenAddr is the HTTP bind address, e.g. ":8443".
	ListenAddr string
	// AdminToken guards /api/admin/*. Empty disables the admin API.
	AdminToken string
	// TrustProxy makes the coordinator honor X-Forwarded-For from a reverse
	// proxy when determining beacon source IPs.
	TrustProxy bool
}

// NewRouter assembles all HTTP routes.
func NewRouter(s *Server, cfg Config) http.Handler {
	mux := http.NewServeMux()

	// Agent WebSocket endpoint.
	mux.HandleFunc("/ws", s.HandleAgentWS)

	// Beacon check-in endpoint (bearer token auth inside handler).
	mux.HandleFunc("/api/beacon", s.HandleBeacon(cfg.TrustProxy))

	// Health probe for load balancers / systemd.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Admin API, guarded by middleware.
	admin := http.NewServeMux()
	admin.HandleFunc("GET /api/admin/status", s.HandleStatus)
	admin.HandleFunc("POST /api/admin/ip", s.HandleSetIP)
	admin.HandleFunc("GET /api/admin/ip-history", s.HandleIPHistory)

	admin.HandleFunc("GET /api/admin/agents", s.HandleListAgents)
	admin.HandleFunc("POST /api/admin/agents", s.HandleCreateAgent)
	admin.HandleFunc("PATCH /api/admin/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.HandleUpdateAgent(w, r, r.PathValue("id"))
	})
	admin.HandleFunc("DELETE /api/admin/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.HandleDeleteAgent(w, r, r.PathValue("id"))
	})
	admin.HandleFunc("POST /api/admin/agents/{id}/rotate", func(w http.ResponseWriter, r *http.Request) {
		s.HandleRotateAgentToken(w, r, r.PathValue("id"))
	})

	admin.HandleFunc("GET /api/admin/beacons", s.HandleListBeacons)
	admin.HandleFunc("POST /api/admin/beacons", s.HandleCreateBeacon)
	admin.HandleFunc("PATCH /api/admin/beacons/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.HandleUpdateBeacon(w, r, r.PathValue("id"))
	})
	admin.HandleFunc("DELETE /api/admin/beacons/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.HandleDeleteBeacon(w, r, r.PathValue("id"))
	})
	admin.HandleFunc("POST /api/admin/beacons/{id}/rotate", func(w http.ResponseWriter, r *http.Request) {
		s.HandleRotateBeaconToken(w, r, r.PathValue("id"))
	})

	mux.Handle("/api/admin/", s.adminAuth(cfg.AdminToken)(admin))

	// Dashboard UI (static, no auth at this layer; the admin API it calls is
	// authed, so an unauthenticated viewer sees an empty login form).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})

	return withLogging(s.log, mux)
}

// withLogging is minimal request logging for non-dashboard routes.
func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip noisy health/beacon polling in logs unless debugging.
		if r.URL.Path == "/healthz" || r.URL.Path == "/api/beacon" {
			next.ServeHTTP(w, r)
			return
		}
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "remote", clientHost(r))
		next.ServeHTTP(w, r)
	})
}

func clientHost(r *http.Request) string {
	if i := strings.LastIndex(r.RemoteAddr, ":"); i != -1 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
