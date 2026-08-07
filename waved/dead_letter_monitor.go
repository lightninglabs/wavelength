package waved

import (
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/metrics"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// startDeadLetterMonitor constructs and starts the dead-letter monitor over
// the daemon's delivery store. The monitor is the daemon-owned consumer
// issue #705 calls for: it logs and counts every newly parked dead letter,
// summarizes the backlog left by previous runs, and applies the configured
// retention policy. It also drives the store's expired-entry cleanup
// (deduplication records and ask results), which previously had no periodic
// caller at all.
func (s *Server) startDeadLetterMonitor() error {
	dlStore, ok := s.deliveryStore.(actor.DeadLetterStore)
	if !ok {
		return fmt.Errorf("delivery store does not implement the " +
			"dead-letter surface")
	}

	s.deadLetterMonitor = actor.NewDeadLetterMonitor(
		actor.DeadLetterMonitorConfig{
			Store:         dlStore,
			Maintenance:   s.deliveryStore,
			ScanInterval:  s.cfg.DeadLetter.ScanInterval,
			SweepInterval: s.cfg.DeadLetter.SweepInterval,
			Retention:     s.cfg.DeadLetter.Retention,
			Clock:         s.clk,
			Log:           fn.Some(s.subLogger(actor.Subsystem)),
			OnDeadLetter: func(_ actor.DeadLetter) {
				// The counter is registered only when the
				// metrics server is enabled; incrementing an
				// unregistered collector is a safe no-op.
				metrics.DeadLettersObservedTotal.Inc()
			},
		},
	)
	s.deadLetterMonitor.Start()

	return nil
}
