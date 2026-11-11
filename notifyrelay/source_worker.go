package notifyrelay

import (
	"context"
	"log/slog"
	"time"
)

// runFeedSource owns one source connection and steps until daemon cancellation.
// Failures back off without dropping committed jobs or pending ACK evidence.
// All calls and cleanup remain inside the daemon-owned goroutine.
func (s *Server) runFeedSource(ctx context.Context, store *SQLStore,
	id string) {

	for ctx.Err() == nil {
		subscriber, err := NewFeedSubscriber(ctx, store, id)
		if err == nil {
			for ctx.Err() == nil {
				worked, stepErr := subscriber.Step(ctx)
				if stepErr != nil {
					break
				}
				if !worked && !waitSource(ctx, time.Second) {
					break
				}
			}
			_ = subscriber.Close()
		}
		if ctx.Err() != nil {
			return
		}
		// No raw driver/network error can expose DSNs, tokens or proof
		// metadata.
		s.sourceFailures.Add(1)
		s.log.DebugContext(
			ctx, "Notification source retry deferred",
			slog.String("source", id),
		)
		if !waitSource(ctx, 5*time.Second) {
			return
		}
	}
}

// waitSource is a cancellation-owned delay; no timer escapes the source loop.
func waitSource(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false

	case <-timer.C:
		return true
	}
}
