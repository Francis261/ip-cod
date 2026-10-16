package coordinator

import (
	"context"
	"time"

	"github.com/ipcod/ipcod/internal/ipdetect"
)

// StartIPDetection runs the outbound public-IP detector in the background
// until ctx is cancelled. Every observed change is committed and broadcast to
// connected agents through the same path a beacon check-in would use, so a
// detected change and a beacon-reported change behave identically.
//
// This is what makes a separate beacon process unnecessary: the coordinator
// lives on the laptop and learns its own public address by asking echo
// services outbound.
func (s *Server) StartIPDetection(ctx context.Context, cfg ipdetect.Config) {
	d := ipdetect.New(cfg, s.log)
	go func() {
		err := d.Run(ctx, func(ip string) {
			if _, _, err := s.observeIP(ctx, ip, "auto"); err != nil {
				s.log.Error("failed to record detected IP", "ip", ip, "err", err)
			}
		})
		if err != nil && ctx.Err() == nil {
			s.log.Warn("ip detector stopped", "err", err)
		}
	}()
}

// DefaultDetectInterval is how often the public IP is checked when the user
// does not configure an interval.
const DefaultDetectInterval = 60 * time.Second
