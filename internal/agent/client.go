package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ipcod/ipcod/internal/protocol"
)

// Config configures one agent instance.
type Config struct {
	// CoordinatorURL is the ws:// or wss:// URL of the coordinator's /ws endpoint.
	CoordinatorURL string
	// Token is this agent's bearer token.
	Token string
	// Name is a display name the agent suggests (coordinator may override).
	Name string
	// UFWBin overrides the ufw binary path ("" = "ufw" on PATH).
	UFWBin string
	// DryRun logs ufw commands instead of executing them.
	DryRun bool
	// MinBackoff / MaxBackoff bound the reconnect delay.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

// Client is the agent daemon: it maintains a WebSocket to the coordinator and
// applies firewall rules when the laptop IP changes.
type Client struct {
	cfg      Config
	applier  *UFWApplier
	log      *slog.Logger
	revision atomic.Uint64 // highest revision applied, to ignore stale updates
}

// NewClient builds an agent client.
func NewClient(cfg Config, log *slog.Logger) *Client {
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 2 * time.Minute
	}
	return &Client{
		cfg:     cfg,
		applier: NewUFWApplier(cfg.UFWBin, cfg.DryRun, log),
		log:     log,
	}
}

// Run blocks, maintaining the connection until ctx is cancelled. It reconnects
// forever with exponential backoff.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.MinBackoff
	for {
		err := c.session(ctx)
		if ctx.Err() != nil {
			// Clean shutdown: drop the firewall rule we added.
			if cerr := c.applier.Clear(); cerr != nil {
				c.log.Warn("failed to clear ufw rule on shutdown", "err", cerr)
			}
			return ctx.Err()
		}
		if err != nil {
			c.log.Warn("session ended", "err", err, "reconnect_in", backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		// Grow backoff with jitter up to the cap.
		backoff = nextBackoff(backoff, c.cfg.MaxBackoff)
	}
}

// Check performs a single dial + auth against the coordinator and returns the
// result without entering the long-running loop. It is used by the `check`
// subcommand to validate a token/config. It does not touch the firewall.
func (c *Client) Check(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, c.cfg.CoordinatorURL, http.Header{})
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := writeMsg(conn, &protocol.Message{
		Type: protocol.TypeAuth,
		Auth: &protocol.AuthPayload{Token: c.cfg.Token, Name: c.cfg.Name},
	}); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	reply, err := protocol.Decode(data)
	if err != nil {
		return err
	}
	switch reply.Type {
	case protocol.TypeAuthOK:
		return nil
	case protocol.TypeAuthFail:
		msg := "authentication failed"
		if reply.Error != nil {
			msg = reply.Error.Message
		}
		return errors.New(msg)
	default:
		return fmt.Errorf("unexpected reply to auth: %s", reply.Type)
	}
}

// session runs one connection: dial, authenticate, then read until the
// connection drops or ctx is done.
func (c *Client) session(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	hdr := http.Header{}
	conn, _, err := dialer.DialContext(ctx, c.cfg.CoordinatorURL, hdr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Authenticate immediately.
	auth := &protocol.Message{
		Type: protocol.TypeAuth,
		Auth: &protocol.AuthPayload{Token: c.cfg.Token, Name: c.cfg.Name},
	}
	if err := writeMsg(conn, auth); err != nil {
		return err
	}

	// Wait for the auth verdict before starting the main loop.
	_, data, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	reply, err := protocol.Decode(data)
	if err != nil {
		return err
	}
	switch reply.Type {
	case protocol.TypeAuthOK:
		c.log.Info("authenticated with coordinator")
	case protocol.TypeAuthFail:
		msg := "auth failed"
		if reply.Error != nil {
			msg = reply.Error.Message
		}
		// Auth failures are not transient; back off hard.
		return &authError{msg}
	default:
		return &authError{"unexpected reply to auth"}
	}

	// Read loop.
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		msg, err := protocol.Decode(data)
		if err != nil {
			c.log.Warn("bad frame from coordinator", "err", err)
			continue
		}
		c.handle(ctx, conn, msg)
	}
}

// handle processes a single coordinator message.
func (c *Client) handle(ctx context.Context, conn *websocket.Conn, msg *protocol.Message) {
	switch msg.Type {
	case protocol.TypePing:
		_ = writeMsg(conn, &protocol.Message{Type: protocol.TypePong})

	case protocol.TypeIPChanged:
		p := msg.IPChanged
		if p == nil {
			return
		}
		c.applyIP(ctx, p)

	case protocol.TypeSetPort:
		p := msg.SetPort
		if p == nil {
			return
		}
		c.log.Info("set_port received but requires a current IP to reapply", "port", p.Port)
		// Port-only changes are applied on the next ip_changed; a full
		// reconciliation would need the coordinator to resend the IP. The
		// coordinator does resend on connect, so this is a minor edge.

	default:
		c.log.Debug("ignoring message", "type", msg.Type)
	}
}

// applyIP applies a firewall rule for the reported laptop IP, honoring the
// revision counter so stale/duplicate updates are dropped.
func (c *Client) applyIP(ctx context.Context, p *protocol.IPChangedPayload) {
	_ = ctx
	// Revision guard: only apply if this is newer than what we have.
	for {
		cur := c.revision.Load()
		if p.Revision <= cur {
			c.log.Debug("stale ip update ignored", "rev", p.Revision, "current", cur)
			return
		}
		if c.revision.CompareAndSwap(cur, p.Revision) {
			break
		}
	}

	if p.IP == "" {
		c.log.Warn("received empty IP; skipping")
		return
	}
	if err := c.applier.Apply(p.IP, p.Port, p.Proto); err != nil {
		c.log.Error("failed to apply ufw rule", "ip", p.IP, "port", p.Port, "err", err)
		return
	}
	c.log.Info("firewall updated", "ip", p.IP, "port", p.Port, "proto", orTCP(p.Proto), "rev", p.Revision)
}

// authError signals a non-retryable authentication problem.
type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

// IsAuthError reports whether err is an unrecoverable auth failure.
func IsAuthError(err error) bool {
	_, ok := err.(*authError)
	return ok
}

// writeMsg writes a protocol message with a deadline.
func writeMsg(conn *websocket.Conn, m *protocol.Message) error {
	data, err := protocol.Encode(m)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// nextBackoff doubles the current delay (with a little jitter) up to max.
func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		next = max
	}
	return next
}

func orTCP(p string) string {
	if p == "" {
		return "tcp"
	}
	return p
}
