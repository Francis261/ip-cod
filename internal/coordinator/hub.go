package coordinator

import (
	"context"
	"log/slog"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/ipcod/ipcod/internal/protocol"
)

// agentConn is a single authenticated agent's live WebSocket connection.
type agentConn struct {
	id    string // agent ID
	name  string
	ports []uint16
	proto string
	conn  *websocket.Conn

	// send is the outbound write queue. Writes happen on a single dedicated
	// goroutine so we never interleave frames on the wire.
	send chan *protocol.Message

	// closed guards close of send; sync.Once ensures we close exactly once.
	closeOnce sync.Once
}

// hub tracks all currently connected agent connections keyed by agent ID and
// fans out IP-change broadcasts.
type hub struct {
	mu    sync.RWMutex
	conns map[string]*agentConn // agentID -> conn
	log   *slog.Logger
}

func newHub(log *slog.Logger) *hub {
	return &hub{
		conns: make(map[string]*agentConn),
		log:   log,
	}
}

// register adds a connection for an agent ID. If the agent is already
// connected (for example it re-authed after a flaky network), the old
// connection is evicted and closed so we never leak goroutines.
func (h *hub) register(ac *agentConn) {
	h.mu.Lock()
	if old, ok := h.conns[ac.id]; ok {
		h.log.Info("evicting stale agent connection", "agent", ac.name, "id", ac.id)
		old.close()
	}
	h.conns[ac.id] = ac
	h.mu.Unlock()
	h.log.Info("agent connected", "agent", ac.name, "id", ac.id, "ports", ac.ports)
}

// unregister removes the connection only if it is still the registered one for
// that agent ID. This avoids a late-disconnecting old socket evicting a newer
// replacement connection.
func (h *hub) unregister(ac *agentConn) {
	h.mu.Lock()
	if cur, ok := h.conns[ac.id]; ok && cur == ac {
		delete(h.conns, ac.id)
		h.mu.Unlock()
		h.log.Info("agent disconnected", "agent", ac.name, "id", ac.id)
		return
	}
	h.mu.Unlock()
}

// setPorts updates the remembered guard ports for a connected agent so future
// broadcasts use the new values.
func (h *hub) setPorts(id string, ports []uint16, proto string) {
	h.mu.Lock()
	if ac, ok := h.conns[id]; ok {
		ac.ports = ports
		ac.proto = proto
	}
	h.mu.Unlock()
}

// connectedIDs returns the set of agent IDs currently connected.
func (h *hub) connectedIDs() map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]bool, len(h.conns))
	for id := range h.conns {
		out[id] = true
	}
	return out
}

// isLive reports whether an agent ID is currently connected.
func (h *hub) isLive(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[id]
	return ok
}

// close shuts down every connection, used on coordinator shutdown.
func (h *hub) close() {
	h.mu.Lock()
	conns := make([]*agentConn, 0, len(h.conns))
	for _, ac := range h.conns {
		conns = append(conns, ac)
	}
	h.conns = make(map[string]*agentConn)
	h.mu.Unlock()
	for _, ac := range conns {
		ac.close()
	}
}

// broadcastIPChange queues an ip_changed message for every connected agent.
// Each agent gets its own copy so the per-agent port is correct. The call does
// not block: if an agent's queue is full (slow/stuck socket) the message is
// dropped and logged, because a stuck agent should not stall the others.
func (h *hub) broadcastIPChange(ip string, revision uint64) {
	h.mu.RLock()
	targets := make([]*agentConn, 0, len(h.conns))
	for _, ac := range h.conns {
		targets = append(targets, ac)
	}
	h.mu.RUnlock()

	for _, ac := range targets {
		msg := &protocol.Message{
			Type: protocol.TypeIPChanged,
			IPChanged: &protocol.IPChangedPayload{
				IP:       ip,
				Port:     primaryPort(ac.ports),
				Ports:    ac.ports,
				Proto:    ac.proto,
				Revision: revision,
			},
		}
		h.enqueue(ac, msg)
	}
}

// sendTo queues a single message to one connected agent by ID.
func (h *hub) sendTo(ctx context.Context, id string, msg *protocol.Message) bool {
	_ = ctx
	h.mu.RLock()
	ac, ok := h.conns[id]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	return h.enqueue(ac, msg)
}

// enqueue non-blockingly puts a message on an agent's send queue.
func (h *hub) enqueue(ac *agentConn, msg *protocol.Message) bool {
	select {
	case ac.send <- msg:
		return true
	default:
		h.log.Warn("dropping message to agent with full send queue",
			"agent", ac.name, "id", ac.id, "type", msg.Type)
		return false
	}
}

// primaryPort returns the first guarded port (0 when the list is empty) — the
// single-port field older agents still understand.
func primaryPort(ports []uint16) uint16 {
	if len(ports) == 0 {
		return 0
	}
	return ports[0]
}

// close tears down a single connection: closes the send queue (once) and the
// underlying socket.
func (ac *agentConn) close() {
	ac.closeOnce.Do(func() {
		close(ac.send)
		// Closing the socket unblocks any pending read on the agent handler.
		_ = ac.conn.Close()
	})
}
