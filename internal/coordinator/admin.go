package coordinator

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ipcod/ipcod/internal/store"
)

// adminAuth middleware guards all /api/admin/* routes with a static admin
// bearer token (from config/env). Simple and sufficient for a single-operator
// dashboard; put the whole thing behind TLS.
func (s *Server) adminAuth(adminToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "admin token not configured"})
				return
			}
			if bearerToken(r) != adminToken {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---- agent management ----

type agentView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Port      uint16 `json:"port"`
	Proto     string `json:"proto"`
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	LastSeen  string `json:"last_seen"`
	LastIP    string `json:"last_ip"`
	CreatedAt string `json:"created_at"`
}

func (s *Server) agentView(a store.Agent) agentView {
	return agentView{
		ID:        a.ID,
		Name:      a.Name,
		Port:      a.Port,
		Proto:     a.Proto,
		Enabled:   a.Enabled,
		Connected: s.hub.isLive(a.ID),
		LastSeen:  formatTime(a.LastSeen),
		LastIP:    a.LastIP,
		CreatedAt: formatTime(a.CreatedAt),
	}
}

// HandleListAgents GET /api/admin/agents
func (s *Server) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	views := make([]agentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, s.agentView(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": views})
}

type createAgentReq struct {
	Name  string `json:"name"`
	Port  uint16 `json:"port"`
	Proto string `json:"proto,omitempty"`
}

// HandleCreateAgent POST /api/admin/agents
// Returns the plaintext token exactly once.
func (s *Server) HandleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || req.Port == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and port are required"})
		return
	}
	a, plain, err := s.store.CreateAgent(r.Context(), req.Name, req.Port, normalizeProto(req.Proto))
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent": s.agentView(*a),
		"token": plain,
		"note":  "Store this token securely. It will not be shown again.",
	})
}

// HandleUpdateAgent PATCH /api/admin/agents/{id}
func (s *Server) HandleUpdateAgent(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Name    *string `json:"name"`
		Port    *uint16 `json:"port"`
		Proto   *string `json:"proto"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name != nil {
		if err := s.store.RenameAgent(r.Context(), id, strings.TrimSpace(*req.Name)); err != nil {
			writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
			return
		}
	}
	if req.Enabled != nil {
		if err := s.store.SetAgentEnabled(r.Context(), id, *req.Enabled); err != nil {
			writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
			return
		}
	}
	if req.Port != nil {
		proto := ""
		if req.Proto != nil {
			proto = normalizeProto(*req.Proto)
		}
		if err := s.SetPort(r.Context(), id, *req.Port, proto); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else if req.Proto != nil {
		ag, err := s.store.GetAgent(r.Context(), id)
		if err != nil {
			writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
			return
		}
		if err := s.SetPort(r.Context(), id, ag.Port, normalizeProto(*req.Proto)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	a, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentView(*a)})
}

// HandleRotateAgentToken POST /api/admin/agents/{id}/rotate
func (s *Server) HandleRotateAgentToken(w http.ResponseWriter, r *http.Request, id string) {
	plain, err := s.store.RotateAgentToken(r.Context(), id)
	if err != nil {
		writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": plain})
}

// HandleDeleteAgent DELETE /api/admin/agents/{id}
func (s *Server) HandleDeleteAgent(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteAgent(r.Context(), id); err != nil {
		writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// ---- beacon management ----

type beaconView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	LastSeen  string `json:"last_seen"`
	LastIP    string `json:"last_ip"`
	CreatedAt string `json:"created_at"`
}

func beaconViewOf(b store.Beacon) beaconView {
	return beaconView{
		ID:        b.ID,
		Name:      b.Name,
		Enabled:   b.Enabled,
		LastSeen:  formatTime(b.LastSeen),
		LastIP:    b.LastIP,
		CreatedAt: formatTime(b.CreatedAt),
	}
}

// HandleListBeacons GET /api/admin/beacons
func (s *Server) HandleListBeacons(w http.ResponseWriter, r *http.Request) {
	bs, err := s.store.ListBeacons(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	views := make([]beaconView, 0, len(bs))
	for _, b := range bs {
		views = append(views, beaconViewOf(b))
	}
	writeJSON(w, http.StatusOK, map[string]any{"beacons": views})
}

// HandleCreateBeacon POST /api/admin/beacons
func (s *Server) HandleCreateBeacon(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	b, plain, err := s.store.CreateBeacon(r.Context(), req.Name)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"beacon": beaconViewOf(*b),
		"token":  plain,
		"note":   "Store this token securely. It will not be shown again.",
	})
}

// HandleUpdateBeacon PATCH /api/admin/beacons/{id}
func (s *Server) HandleUpdateBeacon(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name != nil {
		if err := s.store.RenameBeacon(r.Context(), id, strings.TrimSpace(*req.Name)); err != nil {
			writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
			return
		}
	}
	if req.Enabled != nil {
		if err := s.store.SetBeaconEnabled(r.Context(), id, *req.Enabled); err != nil {
			writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
			return
		}
	}
	b, err := s.store.GetBeacon(r.Context(), id)
	if err != nil {
		writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"beacon": beaconViewOf(*b)})
}

// HandleRotateBeaconToken POST /api/admin/beacons/{id}/rotate
func (s *Server) HandleRotateBeaconToken(w http.ResponseWriter, r *http.Request, id string) {
	plain, err := s.store.RotateBeaconToken(r.Context(), id)
	if err != nil {
		writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": plain})
}

// HandleDeleteBeacon DELETE /api/admin/beacons/{id}
func (s *Server) HandleDeleteBeacon(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteBeacon(r.Context(), id); err != nil {
		writeJSON(w, httpStatusFor(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// ---- status / IP ----

type statusResp struct {
	IP        string `json:"ip"`
	Revision  uint64 `json:"revision"`
	ChangedAt string `json:"changed_at"`
	Connected int    `json:"connected_agents"`
}

// HandleStatus GET /api/admin/status
func (s *Server) HandleStatus(w http.ResponseWriter, r *http.Request) {
	ip, rev, at, ok, err := s.store.CurrentIP(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	changedAt := ""
	if ok {
		changedAt = formatTime(at)
	}
	writeJSON(w, http.StatusOK, statusResp{
		IP:        ip,
		Revision:  rev,
		ChangedAt: changedAt,
		Connected: len(s.hub.connectedIDs()),
	})
}

// HandleSetIP POST /api/admin/ip  { "ip": "1.2.3.4" }
func (s *Server) HandleSetIP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	changed, rev, err := s.SetIP(r.Context(), req.IP)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ip": req.IP, "changed": changed, "revision": rev})
}

// HandleIPHistory GET /api/admin/ip-history
func (s *Server) HandleIPHistory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	hist, err := s.store.IPHistory(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type row struct {
		IP        string `json:"ip"`
		BeaconID  string `json:"beacon_id"`
		ChangedAt string `json:"changed_at"`
	}
	rows := make([]row, 0, len(hist))
	for _, h := range hist {
		rows = append(rows, row{h.IP, h.BeaconID, formatTime(h.ChangedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": rows})
}

// ---- helpers ----

func normalizeProto(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "tcp"
	}
	return p
}

func httpStatusFor(err error) int {
	if err == store.ErrNotFound {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
