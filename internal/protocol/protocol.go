// Package protocol defines the wire format for messages exchanged between the
// coordinator and agents over the WebSocket connection.
package protocol

import "encoding/json"

// MessageType identifies the kind of a Message.
type MessageType string

const (
	// Agent -> Coordinator
	TypeAuth     MessageType = "auth"
	TypePong     MessageType = "pong"
	TypeReportIP MessageType = "report_ip"

	// Coordinator -> Agent
	TypeAuthOK    MessageType = "auth_ok"
	TypeAuthFail  MessageType = "auth_fail"
	TypePing      MessageType = "ping"
	TypeIPChanged MessageType = "ip_changed"
	TypeSetPort   MessageType = "set_port"
	TypeError     MessageType = "error"
)

// Message is the envelope for every WebSocket frame in both directions.
// Exactly one of the payload pointer fields is set, matching Type.
type Message struct {
	Type MessageType `json:"type"`

	// Auth is sent by the agent in a TypeAuth frame.
	Auth *AuthPayload `json:"auth,omitempty"`

	// IPChanged is sent by the coordinator when the laptop IP changes.
	IPChanged *IPChangedPayload `json:"ip_changed,omitempty"`

	// SetPort is sent by the coordinator to change the port the agent guards.
	SetPort *SetPortPayload `json:"set_port,omitempty"`

	// Error is sent by either side to report a non-fatal problem.
	Error *ErrorPayload `json:"error,omitempty"`
}

// AuthPayload carries the agent's authentication token and optional metadata.
type AuthPayload struct {
	Token string `json:"token"`
	// Name is a human-friendly label the agent proposes for itself. The
	// coordinator may override it based on its own records.
	Name string `json:"name,omitempty"`
}

// IPChangedPayload tells the agent the laptop's current public IP and which
// ports to allow it to reach. The coordinator sends it on change, on agent
// connect, and periodically as a state assertion; agents verify their firewall
// still matches and repair it if not.
type IPChangedPayload struct {
	IP string `json:"ip"`
	// Port is the first guarded port, kept for older agents that only
	// understand a single port.
	Port uint16 `json:"port"`
	// Ports is the full list of ports to allow. When empty, receivers fall
	// back to Port (older coordinator).
	Ports []uint16 `json:"ports,omitempty"`
	// Proto is the IP protocol, e.g. "tcp" or "udp". Empty means "tcp".
	Proto string `json:"proto,omitempty"`
	// Revision is a monotonically increasing counter so agents can ignore
	// stale or replayed updates. Periodic re-assertions reuse the current
	// revision; agents only drop strictly older ones.
	Revision uint64 `json:"revision"`
}

// EffectivePorts returns the ports to guard, tolerating payloads from a
// coordinator that only carries the single Port field.
func (p *IPChangedPayload) EffectivePorts() []uint16 {
	switch {
	case len(p.Ports) > 0:
		return p.Ports
	case p.Port > 0:
		return []uint16{p.Port}
	default:
		return nil
	}
}

// SetPortPayload tells the agent to change the ports it guards without
// changing the allowed IP. (Superseded by ip_changed, which now carries the
// ports too; kept for wire compatibility.)
type SetPortPayload struct {
	Port  uint16   `json:"port"`
	Ports []uint16 `json:"ports,omitempty"`
	Proto string   `json:"proto,omitempty"`
}

// ErrorPayload carries a human-readable error description.
type ErrorPayload struct {
	Message string `json:"message"`
}

// Encode marshals a Message to JSON bytes.
func Encode(m *Message) ([]byte, error) {
	return json.Marshal(m)
}

// Decode unmarshals JSON bytes into a Message.
func Decode(b []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
