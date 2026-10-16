package coordinator

import (
	"context"
	"time"
)

// DefaultReconcileInterval is how often the coordinator re-asserts the current
// IP to every connected agent when the user does not configure an interval.
const DefaultReconcileInterval = 30 * time.Second

// StartReconcile periodically re-broadcasts the stored laptop IP (with each
// agent's ports) to all connected agents, until ctx is cancelled. The push is
// an idempotent state assertion: agents compare it against their live firewall
// and only touch the difference, so this doubles as a self-heal loop — rules
// deleted out-of-band are repaired within one interval, and a push an agent
// missed (e.g. its queue was full) is retried. A negative interval disables
// periodic pushes entirely.
func (s *Server) StartReconcile(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n := len(s.hub.connectedIDs())
				if n == 0 {
					continue
				}
				ip, rev, _, ok, err := s.store.CurrentIP(ctx)
				if err != nil || !ok {
					continue
				}
				s.log.Debug("reconcile push", "ip", ip, "revision", rev, "agents", n)
				s.hub.broadcastIPChange(ip, rev)
			}
		}
	}()
}
