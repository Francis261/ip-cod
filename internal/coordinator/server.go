package coordinator

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ipcod/ipcod/internal/protocol"
	"github.com/ipcod/ipcod/internal/store"
	"github.com/ipcod/ipcod/internal/token"
)

const (
	// how long we wait for the initial auth frame before hanging up
	authTimeout = 10 * time.Second
	// how often we send WebSocket pings to agents
	pingInterval = 30 * time.Second
	// how long a write may block before we consider the agent stuck
	writeTimeout = 10 * time.Second
	// max inbound frame size from an agent
	maxFrameSize = 64 * 1024
)

// Server holds the coordinator's shared state and HTTP handlers.
type Server struct {
	store  *store.Store
	hub    *hub
	log    *slog.Logger
	upgrdr websocket.Upgrader
	// onIPChanged is invoked (synchronously, on the caller's goroutine) after
	// a new IP is committed, so the caller can broadcast to the hub.
}

// NewServer wires up a coordinator Server.
func NewServer(st *store.Store, log *slog.Logger) *Server {
	return &Server{
		store: st,
		hub:   newHub(log),
		log:   log,
		upgrdr: websocket.Upgrader{
			// Agents dial in from anywhere; there is no browser origin to
			// check. Origin checks would only matter for browser clients.
			CheckOrigin: func(r *http.Request) bool { return true },
			// Coalesce fragments to keep memory bounded.
		},
	}
}

// Hub exposes the connection registry (used by the admin API to show liveness).
func (s *Server) Hub() *hub { return s.hub }

// Close shuts down all live agent connections.
func (s *Server) Close() { s.hub.close() }

// HandleAgentWS upgrades an HTTP request to a WebSocket and runs the
// authenticate + read/write loops for one agent.
func (s *Server) HandleAgentWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrdr.Upgrade(w, r, nil)
	if err != nil {
		// Upgrader already wrote an HTTP error response.
		s.log.Debug("websocket upgrade failed", "err", err)
		return
	}

	agent, err := s.authenticate(r.Context(), conn)
	if err != nil {
		s.writeFrame(conn, &protocol.Message{
			Type:  protocol.TypeAuthFail,
			Error: &protocol.ErrorPayload{Message: err.Error()},
		})
		_ = conn.Close()
		return
	}

	ac := &agentConn{
		id:    agent.ID,
		name:  agent.Name,
		port:  agent.Port,
		proto: agent.Proto,
		conn:  conn,
		send:  make(chan *protocol.Message, 64),
	}
	s.hub.register(ac)
	defer func() {
		s.hub.unregister(ac)
		ac.close()
	}()

	// Ack auth and, if we already have a laptop IP, push it immediately so a
	// freshly connected agent reconciles its firewall without waiting for the
	// next change.
	if err := s.writeFrame(conn, &protocol.Message{Type: protocol.TypeAuthOK}); err != nil {
		return
	}
	s.pushCurrentIP(context.Background(), ac)

	// Run the read and write loops; whichever finishes first cancels the other.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go s.writeLoop(ctx, ac)
	s.readLoop(ctx, ac, agent)
}

// authenticate reads the first frame and validates it as an agent auth.
func (s *Server) authenticate(ctx context.Context, conn *websocket.Conn) (*store.Agent, error) {
	_ = conn.SetReadDeadline(time.Now().Add(authTimeout))
	conn.SetReadLimit(maxFrameSize)
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, errors.New("failed to read auth frame")
	}
	// Clear the read deadline; the read loop sets its own.
	_ = conn.SetReadDeadline(time.Time{})

	msg, err := protocol.Decode(data)
	if err != nil || msg.Type != protocol.TypeAuth || msg.Auth == nil {
		return nil, errors.New("first frame must be an auth message")
	}

	// Reject beacon tokens used here; agents must present agent tokens.
	if kind, err := token.KindOf(msg.Auth.Token); err != nil || kind != token.KindAgent {
		return nil, errors.New("invalid token type")
	}

	agent, err := s.store.GetAgentByTokenHash(ctx, token.Hash(msg.Auth.Token))
	if err != nil {
		return nil, errors.New("authentication failed")
	}
	if !agent.Enabled {
		return nil, errors.New("agent is disabled")
	}
	return agent, nil
}

// readLoop consumes frames from the agent until the connection drops.
func (s *Server) readLoop(ctx context.Context, ac *agentConn, agent *store.Agent) {
	_ = ctx
	conn := ac.conn
	conn.SetReadLimit(maxFrameSize)
	_ = conn.SetReadDeadline(time.Now().Add(pingInterval * 2))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pingInterval * 2))
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(pingInterval * 2))

		msg, err := protocol.Decode(data)
		if err != nil {
			s.log.Warn("bad frame from agent", "agent", ac.name, "err", err)
			continue
		}
		switch msg.Type {
		case protocol.TypePong:
			// liveness already recorded via read deadline; refresh last_seen
			_ = s.store.TouchAgentSeen(context.Background(), agent.ID)
		case protocol.TypeReportIP:
			// An agent may volunteer the IP it sees for the laptop is not
			// meaningful here; ignore but keep for forward compatibility.
		default:
			s.log.Debug("ignoring unexpected frame", "agent", ac.name, "type", msg.Type)
		}
	}
}

// writeLoop is the single writer for a connection: it drains the send queue and
// also emits periodic pings.
func (s *Server) writeLoop(ctx context.Context, ac *agentConn) {
	conn := ac.conn
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ac.send:
			if !ok {
				// channel closed during shutdown
				return
			}
			if err := s.writeFrame(conn, msg); err != nil {
				return
			}
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				return
			}
		}
	}
}

// writeFrame writes one protocol message with a deadline.
func (s *Server) writeFrame(conn *websocket.Conn, msg *protocol.Message) error {
	data, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// pushCurrentIP sends the stored laptop IP to one agent if we have one.
func (s *Server) pushCurrentIP(ctx context.Context, ac *agentConn) {
	ip, rev, _, ok, err := s.store.CurrentIP(ctx)
	if err != nil || !ok {
		return
	}
	msg := &protocol.Message{
		Type: protocol.TypeIPChanged,
		IPChanged: &protocol.IPChangedPayload{
			IP:       ip,
			Port:     ac.port,
			Proto:    ac.proto,
			Revision: rev,
		},
	}
	s.hub.enqueue(ac, msg)
	// Record what we pushed so the agent's last_ip/last_seen are populated
	// right after connect (they otherwise only fill on an IP change).
	_ = s.store.TouchAgent(ctx, ac.id, ip, rev)
}

// HandleAgentWSPath is the http.HandlerFunc bound for the /ws route.
func (s *Server) HandleAgentWSPath() http.HandlerFunc {
	return s.HandleAgentWS
}
