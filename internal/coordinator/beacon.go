package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/ipcod/ipcod/internal/token"
)

// HandleBeacon is the HTTP endpoint beacons POST to so the coordinator can
// learn their current public IP. Authentication is a bearer token.
//
// The observed source IP is taken from the connection's remote address, or
// from the leftmost X-Forwarded-For entry when trustProxy is enabled (for
// running behind nginx/caddy).
func (s *Server) HandleBeacon(trustProxy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		plain := bearerToken(r)
		if plain == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		bc, err := s.store.GetBeaconByTokenHash(r.Context(), token.Hash(plain))
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		if !bc.Enabled {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "beacon disabled"})
			return
		}

		ip := clientIP(r, trustProxy)
		if ip == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not determine source IP"})
			return
		}

		changed, revision, err := s.observeIP(r.Context(), ip, bc.ID)
		if err != nil {
			s.log.Error("failed to observe beacon IP", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		_ = s.store.TouchBeacon(r.Context(), bc.ID, ip)

		writeJSON(w, http.StatusOK, map[string]any{
			"ip":       ip,
			"changed":  changed,
			"revision": revision,
		})
	}
}

// observeIP records a newly observed laptop IP. If it differs from the stored
// current IP it commits the change, appends history, and broadcasts to all
// connected agents.
func (s *Server) observeIP(ctx context.Context, ip, beaconID string) (changed bool, revision uint64, err error) {
	curIP, curRev, _, hasCur, sErr := s.store.CurrentIP(ctx)
	if sErr != nil {
		return false, 0, sErr
	}
	if hasCur && curIP == ip {
		// Same IP; nothing to push. Return current revision.
		return false, curRev, nil
	}

	newRev, err := s.store.SetCurrentIP(ctx, ip, beaconID)
	if err != nil {
		return false, 0, err
	}
	s.log.Info("laptop IP changed", "from", orNone(curIP, hasCur), "to", ip, "revision", newRev)

	// Fan out to every connected agent. This is best-effort: a down agent
	// reconciles on its next connect via pushCurrentIP.
	s.hub.broadcastIPChange(ip, newRev)
	// Record the push per connected agent so the dashboard's "last IP"
	// reflects what was actually sent. Best-effort; liveness alone already
	// keeps last_seen fresh via pongs.
	for id := range s.hub.connectedIDs() {
		_ = s.store.TouchAgent(ctx, id, ip, newRev)
	}
	return true, newRev, nil
}

// SetIP lets an admin force a specific IP (for testing or manual override)
// without waiting for a beacon. Returns whether it changed and the revision.
func (s *Server) SetIP(ctx context.Context, ip string) (bool, uint64, error) {
	if net.ParseIP(ip) == nil {
		return false, 0, fmt.Errorf("invalid IP address %q", ip)
	}
	return s.observeIP(ctx, ip, "manual")
}

// SetPort updates an agent's guard ports/proto, persists them, and pushes the
// current state to the agent if it is connected. Empty ports or proto mean
// "keep the current value", so a PATCH can change just one of them.
func (s *Server) SetPort(ctx context.Context, agentID string, ports []uint16, proto string) error {
	cur, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		ports = cur.Ports
	}
	ports, err = normalizePorts(ports)
	if err != nil {
		return err
	}
	if proto == "" {
		proto = cur.Proto
	}
	if err := s.store.UpdateAgentPorts(ctx, agentID, ports, proto); err != nil {
		return err
	}
	// Update the in-memory hub entry and push the current IP with new ports.
	s.hub.setPorts(agentID, ports, proto)
	if ac := s.hub.get(agentID); ac != nil {
		s.pushCurrentIP(ctx, ac)
	}
	return nil
}

// get returns the live connection for an agent ID, or nil.
func (h *hub) get(id string) *agentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[id]
}

// bearerToken extracts the token from an Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// clientIP determines the caller's public IP. When trustProxy is set, the
// leftmost X-Forwarded-For entry is trusted (the one added by our proxy).
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Leftmost entry is the original client as appended by our proxy.
			parts := strings.Split(xff, ",")
			if v := strings.TrimSpace(parts[0]); v != "" {
				if ip := net.ParseIP(v); ip != nil {
					return ip.String()
				}
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr may already be a bare IP.
		if ip := net.ParseIP(r.RemoteAddr); ip != nil {
			return ip.String()
		}
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return ""
}

// writeJSON writes a JSON response with a status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func orNone(s string, ok bool) string {
	if !ok {
		return "(none)"
	}
	return s
}
